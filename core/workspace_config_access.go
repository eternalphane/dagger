package core

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/iancoleman/strcase"

	"github.com/dagger/dagger/core/workspace"
	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
	"github.com/dagger/dagger/engine/engineutil"
)

// This file hosts the workspace-config read chain that module-reference
// resolution (moduleref.go) depends on. It previously lived in core/schema;
// moving it here lets package core resolve module refs without a core->schema
// import cycle, while schema callers keep delegating to it. The ws-scoped
// reads live as methods on *Workspace; the ctx-derived lookups that locate
// the current workspace stay package-level.

// WithWorkspaceClientContext stamps owner metadata for host/resource routing;
// the caller's ClientScope remains the only runtime execution authority.
func WithWorkspaceClientContext(ctx context.Context, ws *Workspace) (context.Context, error) {
	if ws.IsValueWorkspace() {
		return ctx, nil
	}
	if ws.ClientID == "" {
		return nil, fmt.Errorf("workspace has no client ID")
	}
	query, err := CurrentQuery(ctx)
	if err != nil {
		return nil, fmt.Errorf("get current query: %w", err)
	}
	clientMetadata, err := query.SpecificClientMetadata(ctx, ws.ClientID)
	if err != nil {
		return ctx, fmt.Errorf("get client metadata: %w", err)
	}
	return engine.ContextWithClientMetadata(ctx, clientMetadata), nil
}

func workspaceBuildkit(ctx context.Context) (*engineutil.Client, error) {
	query, err := CurrentQuery(ctx)
	if err != nil {
		return nil, err
	}
	bk, err := query.Engine(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine client: %w", err)
	}
	return bk, nil
}

// ConfigFilePath returns the workspace-relative path of the workspace's
// dagger.toml.
func (ws *Workspace) ConfigFilePath() (string, error) {
	if ws == nil {
		return "", fmt.Errorf("workspace is required")
	}
	if ws.ConfigFile == "" {
		return "", fmt.Errorf("no dagger.toml found in workspace")
	}
	return filepath.Clean(ws.ConfigFile), nil
}

// requireLocalWorkspace errors unless ws is a local workspace with a host
// path.
func requireLocalWorkspace(ws *Workspace, operation string) error {
	if ws == nil {
		return fmt.Errorf("workspace is required")
	}
	if ws.HostPath() == "" {
		return fmt.Errorf("%s is local-only", operation)
	}
	return nil
}

// HostPathFor returns the absolute host path of rel (workspace-relative)
// under the workspace's local directory.
func (ws *Workspace) HostPathFor(rel ...string) (string, error) {
	if ws == nil {
		return "", fmt.Errorf("workspace is required")
	}
	if err := requireLocalWorkspace(ws, "workspace host access"); err != nil {
		return "", err
	}

	parts := append([]string{ws.HostPath()}, rel...)
	return filepath.Join(parts...), nil
}

// ConfigBytes reads the raw bytes of the workspace's dagger.toml.
func (ws *Workspace) ConfigBytes(ctx context.Context) ([]byte, error) {
	if ws == nil {
		return nil, fmt.Errorf("workspace is required")
	}
	configFile, err := ws.ConfigFilePath()
	if err != nil {
		return nil, err
	}

	if rootfs, ok := ws.SourceDirectory(); ok && rootfs.Self() != nil {
		data, err := DirectoryReadFile(ctx, rootfs, configFile)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		return data, nil
	}

	if ws.HostPath() != "" {
		// Host overlay edits to the config live only in the changeset's delta
		// side (host overlays store no full read root — see overlayEdit);
		// untouched configs read straight from the host file below.
		if deltaRoot, ok := ws.OverlayDeltaRoot(); ok && ws.OverlayPathTouched(configFile) {
			data, err := DirectoryReadFile(ctx, deltaRoot, configFile)
			if err != nil {
				return nil, fmt.Errorf("reading config: %w", err)
			}
			return data, nil
		}

		ctx, err = WithWorkspaceClientContext(ctx, ws)
		if err != nil {
			return nil, err
		}
		configPath, err := ws.HostPathFor(configFile)
		if err != nil {
			return nil, err
		}
		bk, err := workspaceBuildkit(ctx)
		if err != nil {
			return nil, err
		}

		data, err := bk.ReadCallerHostFile(ctx, configPath)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		return data, nil
	}

	rootfs := ws.Rootfs()
	if rootfs.Self() == nil {
		return nil, fmt.Errorf("workspace has no host path or rootfs")
	}
	data, err := DirectoryReadFile(ctx, rootfs, configFile)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return data, nil
}

// Config reads and parses the workspace's dagger.toml.
func (ws *Workspace) Config(ctx context.Context) (*workspace.Config, error) {
	data, err := ws.ConfigBytes(ctx)
	if err != nil {
		return nil, err
	}

	cfg, err := workspace.ParseConfigAt(ctx, data, filepath.Dir(ws.ConfigFile))
	if err != nil {
		return nil, err
	}
	if cfg.Modules == nil {
		cfg.Modules = map[string]workspace.ModuleEntry{}
	}
	return cfg, nil
}

// ConfigWithCompatFallback returns the workspace's config when it has one,
// the shared legacy compat projection when it does not, or an empty config
// for workspaces with neither.
func (ws *Workspace) ConfigWithCompatFallback(ctx context.Context) (*workspace.Config, error) {
	if ws.ConfigFile != "" {
		cfg, err := ws.Config(ctx)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	if compat := ws.CompatWorkspace(); compat != nil {
		return compat.WorkspaceConfig(), nil
	}

	return &workspace.Config{}, nil
}

// currentWorkspaceConfig returns the current query with its workspace and
// config, or ok=false when there is none (errors are deliberately discarded).
func currentWorkspaceConfig(ctx context.Context) (q *Query, ws *Workspace, cfg *workspace.Config, ok bool) {
	q, _ = CurrentQuery(ctx)
	if q == nil {
		return nil, nil, nil, false
	}
	ws, _ = q.Server.CurrentWorkspace(ctx)
	if ws == nil {
		return nil, nil, nil, false
	}
	cfg, _ = ws.ConfigWithCompatFallback(ctx)
	if cfg == nil {
		return nil, nil, nil, false
	}
	return q, ws, cfg, true
}

// demandLoadInstalledModule loads and serves the named workspace module when
// the workspace config installs it but the current command has not loaded it
// (selector verbs narrow module loading to the modules their patterns name).
//
// Returns installed=false when the name is not an installed workspace module —
// the caller's normal address decoding should run unchanged. When installed,
// err reports the load outcome and srv is the refreshed schema served to the
// current client (which now carries the module as a root field).
func demandLoadInstalledModule(ctx context.Context, name string) (srv *dagql.Server, installed bool, err error) {
	q, ws, cfg, ok := currentWorkspaceConfig(ctx)
	if !ok {
		return nil, false, nil
	}
	// Only the workspace-owning client may trigger module loads from address
	// resolution. A module client must never demand-load workspace siblings
	// into its own session — modules only see their declared dependencies,
	// and this gate keeps a bare string from becoming a capability grant.
	md, _ := engine.ClientMetadataFromContext(ctx)
	if md == nil || md.ClientID != ws.ClientID {
		return nil, false, nil
	}
	want := strcase.ToKebab(name)
	for installedName := range cfg.Modules {
		if strcase.ToKebab(installedName) == want {
			installed = true
			break
		}
	}
	if !installed {
		return nil, false, nil
	}
	// Strict (non-best-effort) load: the consumer's constructor requires this
	// module, so a load failure is that resolution's real error. This does not
	// undo best-effort operations like `dagger generate`: their own initial
	// best-effort pass records a failed module, EnsureWorkspaceModules returns
	// the recorded error here without reloading, and ModTree runs nodes
	// without fail-fast — so only the node that genuinely needs the broken
	// module fails, and repair generators keep running.
	if _, err := q.Server.EnsureWorkspaceModules(ctx, []string{name}, ModuleLoadStrict); err != nil {
		return nil, true, err
	}
	deps, err := q.Server.CurrentServedDeps(ctx)
	if err != nil {
		return nil, true, err
	}
	srv, err = deps.Schema(ctx)
	if err != nil {
		return nil, true, err
	}
	return srv, true, nil
}
