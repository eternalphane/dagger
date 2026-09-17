package core

import (
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/stretchr/testify/require"
)

func TestModuleRefShaped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  bool
	}{
		// Long form.
		{"postgres:serve", true},
		// Short (entrypoint) form.
		{"serve", true},
		// URL-ish strings are never module refs.
		{"tcp://localhost:8080", false},
		{"https://github.com/owner/repo", false},
		// Paths are never module refs.
		{"./path", false},
		{"foo/bar:baz", false},
		// Empty segments.
		{":serve", false},
		{"postgres:", false},
		{"", false},
		// Extra segments beyond a single function.
		{"backend:payment:server", false},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, moduleRefShaped(tt.value), tt.value)
	}
}

func TestCheckReturnImplementsInterface(t *testing.T) {
	t.Parallel()

	dag := moduleRefTestDag(t)

	functionResult := func(name string) dagql.ObjectResult[*Function] {
		return objectResultForModuleRefTest(t, dag, "fn-"+name, &Function{
			Name:       name,
			ReturnType: objectResultForModuleRefTest(t, dag, "str", &TypeDef{Kind: TypeDefKindString}),
		})
	}

	interfaceTypeDef := func(name string, fnNames ...string) *InterfaceTypeDef {
		iface := NewInterfaceTypeDef(name, "")
		for _, fnName := range fnNames {
			iface.Functions = append(iface.Functions, functionResult(fnName))
		}
		return iface
	}

	objectTypeDef := func(name string, fnNames ...string) *ObjectTypeDef {
		obj := NewObjectTypeDef(name, "", nil)
		for _, fnName := range fnNames {
			obj.Functions = append(obj.Functions, functionResult(fnName))
		}
		return obj
	}

	refReturning := func(typed dagql.Typed) *ModuleRef {
		return &ModuleRef{
			addr:     "postgres:serve",
			module:   "postgres",
			fnExists: true,
			fnSpec:   dagql.FieldSpec{Type: typed},
			srv:      dag,
		}
	}

	t.Run("object implementing the interface passes", func(t *testing.T) {
		t.Parallel()
		expected := interfaceTypeDef("Store", "serve")
		obj := objectTypeDef("Postgres", "serve", "extra")
		ref := refReturning(&ModuleObject{TypeDef: obj})
		require.NoError(t, ref.checkReturnImplementsInterface(expected))
	})

	t.Run("object missing the interface function fails", func(t *testing.T) {
		t.Parallel()
		expected := interfaceTypeDef("Store", "serve")
		obj := objectTypeDef("Postgres", "load")
		ref := refReturning(&ModuleObject{TypeDef: obj})
		err := ref.checkReturnImplementsInterface(expected)
		require.ErrorContains(t, err, `function returns Postgres, which does not implement Store`)
	})

	t.Run("same interface name passes", func(t *testing.T) {
		t.Parallel()
		expected := interfaceTypeDef("Store", "serve")
		ref := refReturning(&interfaceTypedMarker{name: "Store"})
		require.NoError(t, ref.checkReturnImplementsInterface(expected))
	})

	t.Run("subsuming interface passes", func(t *testing.T) {
		t.Parallel()
		expected := dagql.NewInterface("Store", "")
		expected.AddField(dagql.InterfaceFieldSpec{
			FieldSpec: dagql.FieldSpec{Name: "serve", Type: dagql.String("")},
		})
		returned := dagql.NewInterface("PostgresStore", "")
		returned.AddField(dagql.InterfaceFieldSpec{
			FieldSpec: dagql.FieldSpec{Name: "serve", Type: dagql.String("")},
		})
		dag.InstallInterface(expected)
		dag.InstallInterface(returned)

		expectedIface := interfaceTypeDef("Store", "serve")
		ref := refReturning(&interfaceTypedMarker{name: "PostgresStore"})
		require.NoError(t, ref.checkReturnImplementsInterface(expectedIface))
	})

	t.Run("non-subsuming interface fails", func(t *testing.T) {
		t.Parallel()
		expected := dagql.NewInterface("Store", "")
		expected.AddField(dagql.InterfaceFieldSpec{
			FieldSpec: dagql.FieldSpec{Name: "serve", Type: dagql.String("")},
		})
		returned := dagql.NewInterface("OtherStore", "")
		returned.AddField(dagql.InterfaceFieldSpec{
			FieldSpec: dagql.FieldSpec{Name: "load", Type: dagql.String("")},
		})
		dag.InstallInterface(expected)
		dag.InstallInterface(returned)

		expectedIface := interfaceTypeDef("Store", "serve")
		ref := refReturning(&interfaceTypedMarker{name: "OtherStore"})
		err := ref.checkReturnImplementsInterface(expectedIface)
		require.ErrorContains(t, err, `function returns OtherStore, which does not implement Store`)
	})

	t.Run("uninstalled interface name fails", func(t *testing.T) {
		t.Parallel()
		expectedIface := interfaceTypeDef("Store", "serve")
		ref := refReturning(&interfaceTypedMarker{name: "NotInstalled"})
		err := ref.checkReturnImplementsInterface(expectedIface)
		require.ErrorContains(t, err, "does not implement Store")
	})

	t.Run("non-object non-interface return fails", func(t *testing.T) {
		t.Parallel()
		expected := interfaceTypeDef("Store", "serve")
		ref := refReturning(dagql.String(""))
		err := ref.checkReturnImplementsInterface(expected)
		require.ErrorContains(t, err, "which does not implement Store")
	})
}

func moduleRefTestDag(t *testing.T) *dagql.Server {
	t.Helper()

	dag, err := dagql.NewServer(t.Context(), &Query{})
	require.NoError(t, err)

	dag.InstallObject(dagql.NewClass(dag, dagql.ClassOpts[*TypeDef]{Typed: &TypeDef{}}))
	dag.InstallObject(dagql.NewClass(dag, dagql.ClassOpts[*ListTypeDef]{Typed: &ListTypeDef{}}))
	dag.InstallObject(dagql.NewClass(dag, dagql.ClassOpts[*InterfaceTypeDef]{Typed: &InterfaceTypeDef{}}))
	dag.InstallObject(dagql.NewClass(dag, dagql.ClassOpts[*ObjectTypeDef]{Typed: &ObjectTypeDef{}}))
	dag.InstallObject(dagql.NewClass(dag, dagql.ClassOpts[*Function]{Typed: &Function{}}))
	return dag
}

func objectResultForModuleRefTest[T dagql.Typed](t *testing.T, dag *dagql.Server, op string, self T) dagql.ObjectResult[T] {
	t.Helper()

	res, err := dagql.NewObjectResultForCall(self, dag, &dagql.ResultCall{
		Kind:        dagql.ResultCallKindSynthetic,
		SyntheticOp: "module-ref-test-" + op,
		Type:        dagql.NewResultCallType(self.Type()),
	})
	require.NoError(t, err)
	return res
}
