package protospec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestEdgeScreenSpecMatchesGo catches drift between the Screen structs the Go
// edge decodes and the TypeScript interfaces rendered from edgespec.go. A new
// field, a renamed JSON tag, or a changed Go type must update the spec too.
func TestEdgeScreenSpecMatchesGo(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "internal", "edge", "render.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	goTypes := map[string]*ast.StructType{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, raw := range gd.Specs {
			spec, ok := raw.(*ast.TypeSpec)
			if !ok || (spec.Name.Name != "Screen" && !strings.HasPrefix(spec.Name.Name, "Screen")) {
				continue
			}
			if st, ok := spec.Type.(*ast.StructType); ok {
				goTypes[spec.Name.Name] = st
			}
		}
	}
	if len(goTypes) == 0 {
		t.Fatal("no Screen structs found in internal/edge/render.go")
	}

	specTypes := map[string]EdgeType{}
	for _, spec := range edgeTypes {
		if spec.Name == "Screen" || strings.HasPrefix(spec.Name, "Screen") {
			specTypes[spec.Name] = spec
		}
	}

	for name, st := range goTypes {
		spec, ok := specTypes[name]
		if !ok {
			t.Errorf("%s is missing from edgespec.go", name)
			continue
		}
		fields := st.Fields.List
		if len(fields) != len(spec.Fields) {
			t.Errorf("%s has %d Go fields but %d spec fields", name, len(fields), len(spec.Fields))
			continue
		}
		for i, field := range fields {
			want := spec.Fields[i]
			if len(field.Names) != 1 {
				t.Errorf("%s field %d is not a single named field", name, i)
				continue
			}
			jsonName := field.Names[0].Name
			if field.Tag != nil {
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json")
				if tag != "" {
					jsonName = strings.Split(tag, ",")[0]
				}
			}
			if jsonName != want.Name {
				t.Errorf("%s field %d: Go JSON name %q, spec %q", name, i, jsonName, want.Name)
			}
			wantType, ok := tsType(goTypeName(field.Type))
			if !ok {
				t.Errorf("%s.%s: unsupported Go type %s", name, jsonName, goTypeName(field.Type))
				continue
			}
			if wantType != want.Type {
				t.Errorf("%s.%s: Go type %s renders %q, spec %q", name, jsonName, goTypeName(field.Type), wantType, want.Type)
			}
		}
	}
	for name := range specTypes {
		if _, ok := goTypes[name]; !ok {
			t.Errorf("edgespec.go declares %s but render.go does not", name)
		}
	}
}

func goTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.ArrayType:
		return "[]" + goTypeName(e.Elt)
	case *ast.StarExpr:
		return "*" + goTypeName(e.X)
	case *ast.SelectorExpr:
		return goTypeName(e.X) + "." + e.Sel.Name
	}
	return ""
}

func tsType(goType string) (string, bool) {
	switch goType {
	case "string":
		return "string", true
	case "bool":
		return "boolean", true
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64":
		return "number", true
	}
	if elem, ok := strings.CutPrefix(goType, "[]"); ok {
		inner, ok := tsType(elem)
		if !ok {
			return "", false
		}
		return inner + "[]", true
	}
	// Named struct types keep their name on the TypeScript side.
	if goType != "" && goType[0] >= 'A' && goType[0] <= 'Z' {
		return goType, true
	}
	return "", false
}
