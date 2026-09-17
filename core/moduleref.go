package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/iancoleman/strcase"

	"github.com/dagger/dagger/core/workspace"
	"github.com/dagger/dagger/dagql"
)

// moduleRefCycleKey is the context key carrying the chain of in-flight
// module-reference strings, used to detect reference cycles.
type moduleRefCycleKey struct{}

// ModuleRef is the committed probe result for a module function reference
// "<module>:<function>" (or the short entrypoint form "<function>"): the
// addressing segments, the resolved root field specs, and the (possibly
// demand-refreshed) canonical server and root to select from.
type ModuleRef struct {
	addr          string
	module        string
	moduleField   string
	functionField string
	moduleSpec    dagql.FieldSpec
	fnSpec        dagql.FieldSpec
	fnExists      bool
	srv           *dagql.Server
	root          dagql.AnyObjectResult
}

// ResolveModuleRef detects and resolves a module function reference, wiring
// one module's function output into another object-typed value: the long form
// "<module>:<function>" or the short form "<function>" (a workspace entrypoint function).
//
// Detection & precedence (commit-on-match, no silent fallback):
//   - A long-form candidate contains EXACTLY one ":" with non-empty parts on
//     both sides. Strings containing "://" (URL-ish, e.g. "tcp://...") are
//     never module refs.
//   - A short-form candidate has no ":" or "/" and no leading "." (see
//     workspace.IsShortFormModuleRef). It is committed only if the entrypoint
//     has that function; otherwise it keeps its ordinary address meaning.
//   - The first segment is normalized to a gql field name and looked up on the
//     canonical Query root's object type (the sugared root omits the
//     entrypoint's constructor). Only if a field of that name EXISTS — AND
//     carries module provenance (FieldSpec.Module != nil), which distinguishes
//     a module constructor from a reserved core field like "git" or "secret"
//     that shares the root namespace — is the string committed as a module ref.
//   - Once committed, any subsequent failure (unknown function, type mismatch,
//     cycle) is a HARD error and does NOT fall through to image/URL handling.
//
// Return values:
//   - (true, err): the string was committed as a module ref; err reports the
//     outcome of resolving it (nil on success).
//   - (false, nil): the string is not a module ref; the caller's existing
//     decoding logic should run unchanged.
//
// dest must be a typed dagql destination (e.g. *dagql.ObjectResult[*core.Service])
// so dagql's own typed Select produces the type-mismatch error.
//
// The schema is taken from the context's current dagql server; callers that
// have already resolved a specific serving schema (e.g. the main client's)
// should call probeModuleRef/selectModuleRef directly instead.
func ResolveModuleRef(ctx context.Context, addr string, dest any) (matched bool, err error) {
	srv := dagql.CurrentDagqlServer(ctx)
	if srv == nil {
		return false, nil
	}
	ref, probeCtx, matched, err := probeModuleRef(ctx, srv.Canonical(), addr)
	if !matched || err != nil {
		return matched, err
	}
	return true, selectModuleRef(probeCtx, ref, dest)
}

// probeModuleRef detects whether addr is a module function reference and, once
// committed, resolves its addressing segments against srv without executing
// anything: shape recognition, module ownership (root field with module
// provenance), demand-loading of installed-but-unloaded workspace modules, and
// the reference-cycle guard.
//
// It returns the resolved reference along with a context carrying the extended
// cycle chain (which callers must pass on to selectModuleRef so nested module
// construction can detect re-entry). Return values follow ResolveModuleRef's
// (matched, err) contract.
func probeModuleRef(ctx context.Context, srv *dagql.Server, addr string) (_ *ModuleRef, _ context.Context, matched bool, err error) {
	// URL-ish strings are never module refs.
	if strings.Contains(addr, "://") {
		return nil, nil, false, nil
	}
	module, rest, ok := strings.Cut(addr, ":")
	shortForm := false
	switch {
	case !ok:
		// Short form: resolve "<function>" as "<entrypoint>:<function>".
		if !workspace.IsShortFormModuleRef(addr) {
			return nil, nil, false, nil
		}
		_, _, cfg, found := currentWorkspaceConfig(ctx)
		if !found {
			return nil, nil, false, nil
		}
		entrypoint, err := workspace.EntrypointName(cfg)
		if err != nil || entrypoint == "" {
			return nil, nil, false, nil
		}
		module, rest, shortForm = entrypoint, addr, true
	case module == "" || rest == "":
		return nil, nil, false, nil
	}

	// The entrypoint's constructor only exists on the canonical server.
	root := srv.Root()
	moduleField := strcase.ToLowerCamel(module)
	// Detect whether the module is actually installed by checking the Query
	// root's object type for a field of that name, rather than probing via a
	// Select. If it is not installed, this is not a module ref.
	spec, exists := root.ObjectType().FieldSpec(moduleField, srv.View)
	if !exists {
		// The name may be an installed workspace module that was not loaded
		// for this command: selector verbs (`dagger check <mod>:<item>`)
		// narrow module loading to the modules their patterns name, so a
		// module referenced only through another module's wiring is never
		// loaded. When the workspace config installs a module by this name,
		// demand-load it and retry against the refreshed client schema.
		// Loading is gated on config membership, so image refs (postgres:16)
		// never trigger module loads.
		refreshed, installed, loadErr := demandLoadInstalledModule(ctx, module)
		if !installed {
			return nil, nil, false, nil
		}
		if loadErr != nil {
			// Committed: the workspace installs this module, so the string is
			// a module ref and the load failure is the real error.
			return nil, nil, true, fmt.Errorf("resolve module reference %q: load module %q: %w", addr, module, loadErr)
		}
		srv = refreshed.Canonical()
		root = srv.Root()
		spec, exists = root.ObjectType().FieldSpec(moduleField, srv.View)
		if !exists {
			return nil, nil, false, nil
		}
	}
	// Core Query fields (host, git, secret, engine, container, http, module, ...)
	// share the Query root's namespace with module entrypoints, but only
	// module entrypoints carry module provenance (spec.Module). A field with no
	// Module is a core field — a reserved word — so leave it to the caller's normal
	// address decoding (e.g. "git:2.40" or "secret:foo" as an image/URL) rather
	// than committing it as a module ref.
	if spec.Module == nil {
		return nil, nil, false, nil
	}

	// Committed: from here on, any error is a hard module-ref error.

	// Only "<module>:<function>" (a single function segment) is supported today.
	// A matching module prefix followed by extra colons (e.g.
	// "backend:payment:server") is reported explicitly rather than silently
	// treated as an image ref.
	if strings.Contains(rest, ":") {
		return nil, nil, true, fmt.Errorf("invalid module reference %q: only %s:<function> is supported today (a single function segment); got extra segments in %q", addr, module, rest)
	}
	functionField := strcase.ToLowerCamel(rest)

	// An unknown function is a hard error for the long form (reported by the
	// typed Select below) but means "not a module ref" for the short form.
	var fnSpec dagql.FieldSpec
	fnExists := false
	if objType, ok := srv.ObjectType(spec.Type.Type().Name()); ok {
		fnSpec, fnExists = objType.FieldSpec(functionField, srv.View)
	}
	if shortForm && !fnExists {
		return nil, nil, false, nil
	}

	// Cycle guard: track the chain of in-flight module refs on the context
	// and refuse to descend into one already present. Context values propagate
	// through dagql Select into nested module construction, so re-entry of an
	// in-flight ref is detectable here. Without this, reference cycles hang the
	// engine with unbounded goroutine growth.
	//
	// The chain stores the NORMALIZED "<moduleField>:<functionField>" (both
	// lower-camel), not the raw addr, so equivalently-spelled refs (e.g. case
	// variants like "Foo:Bar" vs "foo:bar") still collide and produce the clean
	// cycle error instead of wedging on a cache wait. The raw addr is kept in the
	// user-facing message for readability.
	normalized := moduleField + ":" + functionField
	chain, _ := ctx.Value(moduleRefCycleKey{}).([]string)
	for _, seen := range chain {
		if seen == normalized {
			return nil, nil, true, fmt.Errorf("module reference cycle detected: %s -> %s",
				strings.Join(chain, " -> "), normalized)
		}
	}
	newChain := make([]string, len(chain)+1)
	copy(newChain, chain)
	newChain[len(chain)] = normalized
	ctx = context.WithValue(ctx, moduleRefCycleKey{}, newChain)

	return &ModuleRef{
		addr:          addr,
		module:        module,
		moduleField:   moduleField,
		functionField: functionField,
		moduleSpec:    spec,
		fnSpec:        fnSpec,
		fnExists:      fnExists,
		srv:           srv,
		root:          root,
	}, ctx, true, nil
}

// selectModuleRef executes a probed module function reference against its
// server: first the module field, then the function field, selecting into dest.
//
// dest must be a typed dagql destination (e.g. *dagql.ObjectResult[*core.Service])
// so dagql's own typed Select produces the type-mismatch error.
func selectModuleRef(ctx context.Context, ref *ModuleRef, dest any) error {
	// Resolve by selecting from the Query root into the typed destination: first
	// the module field, then the function field. dagql's typed Select enforces
	// that the function's return type matches dest, producing a clear
	// type-mismatch error.
	//
	// Both selectors are built by hand, so a required Workspace! on the
	// constructor or the function has to be supplied here: dagql rejects a
	// missing non-null argument in preselect, before the injection hook that
	// fills workspace args runs (see WithBoundWorkspaceArgs). The value
	// resolves the same way it does everywhere else — the workspace bound into
	// the context, else the session's current one.
	ctorArgs := WithBoundWorkspaceArgs(ctx, ref.srv, ref.moduleSpec.Args.Inputs(ref.srv.View), nil)
	var fnArgs []dagql.NamedInput
	if ref.fnExists {
		fnArgs = WithBoundWorkspaceArgs(ctx, ref.srv, ref.fnSpec.Args.Inputs(ref.srv.View), nil)
	}
	selectors := []dagql.Selector{
		{Field: ref.moduleField, Args: ctorArgs},
		{Field: ref.functionField, Args: fnArgs},
	}
	if err := ref.srv.Select(ctx, ref.root, dest, selectors...); err != nil {
		return fmt.Errorf("resolve module reference %q (module %q): %w", ref.addr, ref.module, err)
	}
	return nil
}
