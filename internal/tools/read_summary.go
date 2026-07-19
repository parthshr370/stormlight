package tools

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

const binaryProbeBytes = 8 * 1024

// containsNUL reports whether the leading portion of data identifies it as binary.
func containsNUL(data []byte) bool {
	if len(data) > binaryProbeBytes {
		data = data[:binaryProbeBytes]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// goSourceOutline renders a deterministic, body-free declaration outline for a
// parseable Go source file. Every top-level declaration points at its original
// source region so callers can recover the complete raw text with read selectors.
func goSourceOutline(displayPath string, sourcePath string, data []byte) (string, bool) {
	if filepath.Ext(sourcePath) != ".go" {
		return "", false
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, sourcePath, data, parser.ParseComments)
	if err != nil {
		return "", false
	}

	var outline strings.Builder
	outline.WriteString("package ")
	outline.WriteString(file.Name.Name)
	outline.WriteString(" (line ")
	outline.WriteString(strconv.Itoa(fset.Position(file.Package).Line))
	outline.WriteString(")")
	if len(file.Imports) > 0 {
		outline.WriteString("\nimports:")
		for _, spec := range file.Imports {
			outline.WriteString("\n  ")
			outline.WriteString(renderNode(fset, spec))
			outline.WriteString(" (line ")
			outline.WriteString(strconv.Itoa(fset.Position(spec.Pos()).Line))
			outline.WriteString(")")
		}
	}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		start := fset.Position(decl.Pos()).Line
		end := fset.Position(decl.End()).Line
		outline.WriteByte('\n')
		outline.WriteString(strconv.Itoa(start))
		outline.WriteString(": ")
		outline.WriteString(outlineDeclaration(fset, decl))
		outline.WriteString(" [recover: ")
		outline.WriteString(displayPath)
		outline.WriteByte(':')
		outline.WriteString(strconv.Itoa(start))
		outline.WriteByte('-')
		outline.WriteString(strconv.Itoa(end))
		outline.WriteByte(']')
	}
	return outline.String(), true
}

// outlineDeclaration renders a top-level declaration as a body-free signature:
// functions keep receiver/params/results; type declarations keep their
// underlying signature (struct and interface bodies elided, aliases and named
// types intact, generic parameters preserved); const/var declarations keep
// names, types, and short initializers.
func outlineDeclaration(fset *token.FileSet, decl ast.Decl) string {
	if fn, ok := decl.(*ast.FuncDecl); ok {
		var signature strings.Builder
		signature.WriteString("func ")
		if fn.Recv != nil {
			signature.WriteString(renderParamList(fset, fn.Recv, "(", ")"))
			signature.WriteByte(' ')
		}
		signature.WriteString(fn.Name.Name)
		signature.WriteString(renderSignature(fset, fn.Type))
		return signature.String()
	}
	gen, ok := decl.(*ast.GenDecl)
	if !ok {
		return "<unprintable declaration>"
	}
	specs := make([]string, 0, len(gen.Specs))
	for _, spec := range gen.Specs {
		switch typed := spec.(type) {
		case *ast.TypeSpec:
			var part strings.Builder
			part.WriteString(typed.Name.Name)
			if typed.TypeParams != nil {
				part.WriteString(renderParamList(fset, typed.TypeParams, "[", "]"))
			}
			if typed.Assign.IsValid() {
				part.WriteString(" = ")
			} else {
				part.WriteByte(' ')
			}
			part.WriteString(renderType(fset, typed.Type))
			specs = append(specs, part.String())
		case *ast.ValueSpec:
			var part strings.Builder
			for index, name := range typed.Names {
				if index > 0 {
					part.WriteString(", ")
				}
				part.WriteString(name.Name)
			}
			if typed.Type != nil {
				part.WriteByte(' ')
				part.WriteString(renderType(fset, typed.Type))
			}
			if len(typed.Values) > 0 {
				part.WriteString(" = ")
				part.WriteString(renderValues(fset, typed.Values))
			}
			specs = append(specs, part.String())
		}
	}
	if len(specs) == 0 {
		return gen.Tok.String()
	}
	return gen.Tok.String() + " " + strings.Join(specs, "; ")
}

// renderSignature renders a function type's type parameters, parameters, and
// results with body-free types.
func renderSignature(fset *token.FileSet, fn *ast.FuncType) string {
	var b strings.Builder
	if fn.TypeParams != nil {
		b.WriteString(renderParamList(fset, fn.TypeParams, "[", "]"))
	}
	b.WriteString(renderParamList(fset, fn.Params, "(", ")"))
	if fn.Results != nil && len(fn.Results.List) > 0 {
		b.WriteByte(' ')
		if len(fn.Results.List) == 1 && len(fn.Results.List[0].Names) == 0 {
			b.WriteString(renderType(fset, fn.Results.List[0].Type))
		} else {
			b.WriteString(renderParamList(fset, fn.Results, "(", ")"))
		}
	}
	return b.String()
}

// renderParamList renders a receiver, parameter, result, or type-parameter list
// with the given delimiters and body-free field types.
func renderParamList(fset *token.FileSet, fields *ast.FieldList, left, right string) string {
	if fields == nil {
		return left + right
	}
	parts := make([]string, 0, len(fields.List))
	for _, field := range fields.List {
		var part strings.Builder
		for index, name := range field.Names {
			if index > 0 {
				part.WriteString(", ")
			}
			part.WriteString(name.Name)
		}
		if len(field.Names) > 0 {
			part.WriteByte(' ')
		}
		part.WriteString(renderType(fset, field.Type))
		parts = append(parts, part.String())
	}
	return left + strings.Join(parts, ", ") + right
}

// renderType renders a type expression as a compact, body-free string: struct
// and interface member bodies are elided at every nesting depth while all other
// type syntax (pointers, slices, arrays, maps, channels, funcs, generics, and
// qualified names) is preserved.
func renderType(fset *token.FileSet, expr ast.Expr) string {
	switch t := expr.(type) {
	case nil:
		return ""
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return renderType(fset, t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + renderType(fset, t.X)
	case *ast.ParenExpr:
		return "(" + renderType(fset, t.X) + ")"
	case *ast.Ellipsis:
		return "..." + renderType(fset, t.Elt)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + renderType(fset, t.Elt)
		}
		return "[" + renderNode(fset, t.Len) + "]" + renderType(fset, t.Elt)
	case *ast.MapType:
		return "map[" + renderType(fset, t.Key) + "]" + renderType(fset, t.Value)
	case *ast.ChanType:
		switch t.Dir {
		case ast.SEND:
			return "chan<- " + renderType(fset, t.Value)
		case ast.RECV:
			return "<-chan " + renderType(fset, t.Value)
		default:
			return "chan " + renderType(fset, t.Value)
		}
	case *ast.FuncType:
		return "func" + renderSignature(fset, t)
	case *ast.StructType:
		if n := fieldCount(t.Fields); n > 0 {
			return fmt.Sprintf("struct{ /* %d fields */ }", n)
		}
		return "struct{}"
	case *ast.InterfaceType:
		if n := fieldCount(t.Methods); n > 0 {
			return fmt.Sprintf("interface{ /* %d members */ }", n)
		}
		return "interface{}"
	case *ast.IndexExpr:
		return renderType(fset, t.X) + "[" + renderType(fset, t.Index) + "]"
	case *ast.IndexListExpr:
		args := make([]string, 0, len(t.Indices))
		for _, index := range t.Indices {
			args = append(args, renderType(fset, index))
		}
		return renderType(fset, t.X) + "[" + strings.Join(args, ", ") + "]"
	case *ast.UnaryExpr:
		if t.Op == token.TILDE {
			return "~" + renderType(fset, t.X)
		}
		return renderNode(fset, expr)
	case *ast.BinaryExpr:
		if t.Op == token.OR {
			return renderType(fset, t.X) + " | " + renderType(fset, t.Y)
		}
		return renderNode(fset, expr)
	default:
		return renderNode(fset, expr)
	}
}

// fieldCount counts declared members in a field list; an embedded or anonymous
// entry counts as one member.
func fieldCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	count := 0
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			count++
			continue
		}
		count += len(field.Names)
	}
	return count
}

// renderValues renders initializer expressions with bodies elided (composite
// and function literals) and any other long value truncated, so const/var
// signatures stay compact.
func renderValues(fset *token.FileSet, values []ast.Expr) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, renderValue(fset, value))
	}
	return strings.Join(parts, ", ")
}

// renderValue renders a single initializer, eliding composite-literal and
// function-literal bodies (which can embed anonymous struct/interface fields)
// and truncating any other long value.
func renderValue(fset *token.FileSet, value ast.Expr) string {
	switch v := value.(type) {
	case *ast.CompositeLit:
		if v.Type != nil {
			return renderType(fset, v.Type) + "{…}"
		}
		return "{…}"
	case *ast.FuncLit:
		return "func" + renderSignature(fset, v.Type) + " {…}"
	default:
		rendered := renderNode(fset, value)
		if len(rendered) > 60 {
			rendered = "…"
		}
		return rendered
	}
}

// renderNode keeps fallback AST printing on the same single-line form as a read summary.
func renderNode(fset *token.FileSet, node ast.Node) string {
	var out bytes.Buffer
	if err := printer.Fprint(&out, fset, node); err != nil {
		return "<unprintable declaration>"
	}
	return strings.ReplaceAll(out.String(), "\n", " ")
}
