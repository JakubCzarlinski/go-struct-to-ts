package go2ts

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JakubCzarlinski/go-logging"
	"github.com/JakubCzarlinski/go-pooling"
	"github.com/fatih/structtag"
)

const tsIndent = "  "
const tsTypePrefix = "export"

var rootDirName = ""

type stringBuilder struct {
	*strings.Builder
}

func (b stringBuilder) Reset(struct{}) {
	b.Builder.Reset()
}

var stringBuilderPool = pooling.NewPool(func() *stringBuilder {
	builder := &strings.Builder{}
	builder.Grow(256)
	return &stringBuilder{builder}
})

func SetRootDirName(name string) {
	rootDirName = name
}

func convertType(s string) string {
	switch s {
	case "bool":
		return "boolean"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64",
		"complex64", "complex128":
		return "number"
	}

	return s
}

func writeType(builder *stringBuilder, t ast.Expr, depth int, optionalParens bool) error {
	switch t := t.(type) {

	case *ast.StarExpr:
		if optionalParens {
			builder.WriteByte('(')
		}
		err := writeType(builder, t.X, depth, false)
		if err != nil {
			return logging.Bubble(err, "writeType StarExpr")
		}
		builder.WriteString(" | undefined")
		if optionalParens {
			builder.WriteByte(')')
		}

	case *ast.ArrayType:
		if v, ok := t.Elt.(*ast.Ident); ok && v.String() == "byte" {
			builder.WriteString("string")
			break
		}
		err := writeType(builder, t.Elt, depth, true)
		if err != nil {
			return logging.Bubble(err, "writeType ArrayType")
		}
		builder.WriteString("[]")

	case *ast.StructType:
		builder.WriteString("{\n")
		writeFields(builder, t.Fields.List, depth+1)

		for i := 0; i < depth+1; i++ {
			builder.WriteString(tsIndent)
		}
		builder.WriteByte('}')
		builder.WriteByte(';')

	case *ast.Ident:
		builder.WriteString(convertType(t.String()))

	case *ast.SelectorExpr:
		longType := fmt.Sprintf("%s.%s", t.X, t.Sel)
		switch longType {
		case "time.Time":
			builder.WriteString("string")
		case "time.Duration":
			builder.WriteString("number")
		case "decimal.Decimal":
			builder.WriteString("number")
		case "pgtype.Timestamptz":
			builder.WriteString("string")
		default:
			builder.WriteString(longType)
		}

	case *ast.MapType:
		builder.WriteString("{ [key: ")
		err := writeType(builder, t.Key, depth, false)
		if err != nil {
			return logging.Bubble(err, "writeType MapType Key")
		}
		builder.WriteString("]: ")
		err = writeType(builder, t.Value, depth, false)
		if err != nil {
			return logging.Bubble(err, "writeType MapType Value")
		}
		builder.WriteByte('}')

	case *ast.InterfaceType:
		builder.WriteString("any")

	case *ast.IndexListExpr:
		// Generic type
		builder.WriteString("any")

	default:
		err := fmt.Errorf("unhandled: %s, %T", t, t)
		return logging.Bubble(err, "writeType")

	}
	return nil
}

var validJSNameRegexp = regexp.MustCompile(`(?m)^[\pL_][\pL\pN_]*$`)

func validJSName(n string) bool {
	return validJSNameRegexp.MatchString(n)
}

func writeFields(builder *stringBuilder, fields []*ast.Field, depth int) error {
	for _, f := range fields {
		optional := false

		var fieldName string
		if len(f.Names) != 0 && f.Names[0] != nil && len(f.Names[0].Name) != 0 {
			fieldName = f.Names[0].Name
		}
		if len(fieldName) == 0 || 'A' > fieldName[0] || fieldName[0] > 'Z' {
			continue
		}

		var name string
		if f.Tag != nil {
			tags, err := structtag.Parse(f.Tag.Value[1 : len(f.Tag.Value)-1])
			if err != nil {
				return logging.Bubble(err, "could not parse struct tag")
			}

			jsonTag, err := tags.Get("json")
			if err == nil {
				name = jsonTag.Name
				if name == "-" {
					continue
				}

				optional = jsonTag.HasOption("omitempty")
			}
		}

		if len(name) == 0 {
			name = fieldName
		}

		for i := 0; i < depth+1; i++ {
			builder.WriteString(tsIndent)
		}

		quoted := !validJSName(name)

		if quoted {
			builder.WriteByte('\'')
		}
		builder.WriteString(name)
		if quoted {
			builder.WriteByte('\'')
		}

		switch t := f.Type.(type) {
		case *ast.StarExpr:
			optional = true
			f.Type = t.X
		}

		if optional {
			builder.WriteByte('?')
		}

		builder.WriteString(": ")
		writeType(builder, f.Type, depth, false)
		builder.WriteString(";\n")
	}
	return nil
}

const wrapper = `package main

func main() {
	%s
}`

func Convert(goStructs string) (string, error) {
	if rootDirName == "" {
		return "", logging.Bubble(nil, "RootDirName is empty")
	}

	goStructs = strings.TrimSpace(goStructs)
	if len(goStructs) == 0 {
		return goStructs, nil
	}

	fileSet := token.NewFileSet()
	var parsed ast.Node
	parsed, err := parser.ParseExprFrom(fileSet, "editor.go", goStructs, parser.ParseComments)
	if err != nil {
		parsed, err = parser.ParseFile(fileSet, "editor.go", goStructs, parser.ParseComments)
		if err != nil {
			goStructs = fmt.Sprintf(wrapper, goStructs)
			parsed, err = parser.ParseFile(fileSet, "editor.go", goStructs, parser.ParseComments)
			if err != nil {
				return "", logging.Bubble(err, "could not parse Go code")
			}
		}
	}

	ts := stringBuilderPool.Get()
	defer stringBuilderPool.Reset(ts, struct{}{})
	name := ""

	first := true
	lastWasComment := false

	var builderErr error
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch x := n.(type) {

		case *ast.ImportSpec:
			var importName string
			if x.Name != nil {
				importName = strings.Trim(x.Name.Name, `"`)
			} else {
				importName = filepath.Base(strings.Trim(x.Path.Value, `"`))
			}

			importPath := strings.Trim(x.Path.Value, `"`)
			importPath = strings.Replace(importPath, rootDirName, "@", 1)

			ts.WriteString(fmt.Sprintf("import * as %s from \"%s/types.go.ts\";\n", importName, importPath))
			return false

		case *ast.Ident:
			name = x.Name

		case *ast.Comment:
			if !first {
				ts.WriteString("\n\n")
			}
			ts.WriteString(x.Text + "\n")
			lastWasComment = true
			return false

		case *ast.ArrayType:
			if !first {
				ts.WriteString("\n\n")
			}

			ts.WriteString(fmt.Sprintf("%s type ", tsTypePrefix))
			ts.WriteString(name)
			ts.WriteString(" extends Array<")
			ts.WriteString(fmt.Sprintf("%s", x.Elt))
			ts.WriteString(">{}")
			return false

		case *ast.TypeSpec:
			if _, ok := x.Type.(*ast.InterfaceType); ok {
				ts.WriteString(fmt.Sprintf("%s interface ", tsTypePrefix))
				ts.WriteString(x.Name.Name)
				ts.WriteString(" {\n")
				ts.WriteString("}\n")

				return false
			}

			if !first && !lastWasComment {
				ts.WriteString("\n\n")
			}

			ts.WriteString(fmt.Sprintf("%s type ", tsTypePrefix))
			ts.WriteString(x.Name.Name)
			ts.WriteString(" = ")

			err = writeType(ts, x.Type, -1, false)
			if err != nil {
				builderErr = err
				return false
			}

			first = false
			return false
		}

		return true
	})
	if builderErr != nil {
		return "", logging.Bubble(builderErr, "could not write type")
	}

	return ts.String(), nil
}
