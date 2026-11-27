// Command vibejson-gen generates zero-unsafe, zero-reflection MarshalVibeJSON
// and UnmarshalVibeJSON methods implementing vibejson.MarshalerSimd and
// vibejson.UnmarshalerSimd for Go struct types.
//
// Usage:
//
//	//go:generate go run github.com/thesyncim/vibejson/cmd/vibejson-gen -type=User,Address
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

func main() {
	typeNames := flag.String("type", "", "comma-separated list of struct type names; must be set")
	dir := flag.String("dir", ".", "directory containing the Go package")
	output := flag.String("output", "", "output file name; default <dir>/<first_type>_vibejson.go")
	flag.Parse()

	if strings.TrimSpace(*typeNames) == "" {
		fmt.Fprintln(os.Stderr, "vibejson-gen: -type flag is required")
		os.Exit(2)
	}
	var names []string
	for _, raw := range strings.Split(*typeNames, ",") {
		if n := strings.TrimSpace(raw); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "vibejson-gen: -type flag must name at least one struct")
		os.Exit(2)
	}

	src, err := generateDir(*dir, names)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibejson-gen: %v\n", err)
		os.Exit(1)
	}

	outPath := *output
	if outPath == "" {
		outPath = filepath.Join(*dir, strings.ToLower(names[0])+"_vibejson.go")
	}
	if err := os.WriteFile(outPath, src, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "vibejson-gen: write %s: %v\n", outPath, err)
		os.Exit(1)
	}
}

type fieldKind int

const (
	kindBool fieldKind = iota + 1
	kindString
	kindInt
	kindUint
	kindFloat32
	kindFloat64
	kindStruct
	kindSlice
	kindArray
	kindPointer
)

type fieldType struct {
	kind     fieldKind
	typeName string
	elem     *fieldType
	arrayLen int
}

type structField struct {
	goName    string
	jsonName  string
	omitEmpty bool
	typ       fieldType
}

type structSpec struct {
	name   string
	fields []structField
}

func generateDir(dir string, typeNames []string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var pkgName string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if pkgName == "" {
			pkgName = f.Name.Name
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go source files found in %s", dir)
	}
	return generateFiles(pkgName, files, typeNames)
}

func generateFiles(pkgName string, files []*ast.File, typeNames []string) ([]byte, error) {
	knownStructs := make(map[string]bool, len(typeNames))
	for _, n := range typeNames {
		knownStructs[n] = true
	}

	astStructs := make(map[string]*ast.StructType)
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if st, ok := ts.Type.(*ast.StructType); ok && knownStructs[ts.Name.Name] {
					astStructs[ts.Name.Name] = st
				}
			}
		}
	}

	var specs []structSpec
	for _, name := range typeNames {
		st, ok := astStructs[name]
		if !ok {
			return nil, fmt.Errorf("struct type %q not found", name)
		}
		parsed, err := parseStructSpec(name, st, knownStructs)
		if err != nil {
			return nil, err
		}
		specs = append(specs, parsed)
	}

	return renderPackage(pkgName, specs)
}

func parseStructSpec(name string, st *ast.StructType, knownStructs map[string]bool) (structSpec, error) {
	spec := structSpec{name: name}
	if st.Fields == nil {
		return spec, nil
	}
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return spec, fmt.Errorf("struct %s: embedded fields are not supported by vibejson-gen", name)
		}
		var tag reflect.StructTag
		if f.Tag != nil {
			unquoted, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				return spec, fmt.Errorf("struct %s: invalid tag %s: %w", name, f.Tag.Value, err)
			}
			tag = reflect.StructTag(unquoted)
		}
		jsonTag := tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		ft, err := parseFieldType(f.Type, knownStructs)
		if err != nil {
			return spec, fmt.Errorf("struct %s.%s: %w", name, f.Names[0].Name, err)
		}
		for _, ident := range f.Names {
			if !ast.IsExported(ident.Name) {
				continue
			}
			jsonName, omitEmpty, err := parseJSONTag(ident.Name, jsonTag)
			if err != nil {
				return spec, fmt.Errorf("struct %s.%s: %w", name, ident.Name, err)
			}
			spec.fields = append(spec.fields, structField{
				goName:    ident.Name,
				jsonName:  jsonName,
				omitEmpty: omitEmpty,
				typ:       ft,
			})
		}
	}
	return spec, nil
}

func parseJSONTag(goName, tag string) (name string, omitEmpty bool, err error) {
	if tag == "" {
		return goName, false, nil
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = goName
	}
	for _, opt := range parts[1:] {
		switch opt {
		case "":
		case "omitempty":
			omitEmpty = true
		default:
			return "", false, fmt.Errorf("unsupported json tag option %q", opt)
		}
	}
	return name, omitEmpty, nil
}

func parseFieldType(expr ast.Expr, knownStructs map[string]bool) (fieldType, error) {
	switch e := expr.(type) {
	case *ast.Ident:
		switch e.Name {
		case "bool":
			return fieldType{kind: kindBool, typeName: "bool"}, nil
		case "string":
			return fieldType{kind: kindString, typeName: "string"}, nil
		case "int", "int8", "int16", "int32", "int64":
			return fieldType{kind: kindInt, typeName: e.Name}, nil
		case "uint", "uint8", "uint16", "uint32", "uint64", "byte":
			return fieldType{kind: kindUint, typeName: e.Name}, nil
		case "float32":
			return fieldType{kind: kindFloat32, typeName: "float32"}, nil
		case "float64":
			return fieldType{kind: kindFloat64, typeName: "float64"}, nil
		default:
			if knownStructs[e.Name] {
				return fieldType{kind: kindStruct, typeName: e.Name}, nil
			}
			return fieldType{}, fmt.Errorf("unsupported field type %q (add it to -type if it is a struct in this package)", e.Name)
		}
	case *ast.ArrayType:
		elem, err := parseFieldType(e.Elt, knownStructs)
		if err != nil {
			return fieldType{}, err
		}
		if e.Len == nil {
			return fieldType{kind: kindSlice, typeName: "[]" + elem.typeName, elem: &elem}, nil
		}
		lit, ok := e.Len.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return fieldType{}, fmt.Errorf("unsupported non-literal array length")
		}
		n, err := strconv.Atoi(lit.Value)
		if err != nil || n < 0 {
			return fieldType{}, fmt.Errorf("invalid array length %q", lit.Value)
		}
		return fieldType{kind: kindArray, typeName: fmt.Sprintf("[%d]%s", n, elem.typeName), elem: &elem, arrayLen: n}, nil
	case *ast.StarExpr:
		elem, err := parseFieldType(e.X, knownStructs)
		if err != nil {
			return fieldType{}, err
		}
		return fieldType{kind: kindPointer, typeName: "*" + elem.typeName, elem: &elem}, nil
	default:
		return fieldType{}, fmt.Errorf("unsupported field expression %T", expr)
	}
}

func renderPackage(pkgName string, specs []structSpec) ([]byte, error) {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "// Code generated by vibejson-gen; DO NOT EDIT.")
	fmt.Fprintln(&buf)
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)
	if pkgName != "vibejson" {
		fmt.Fprintln(&buf, `import "github.com/thesyncim/vibejson"`)
		fmt.Fprintln(&buf)
	}

	qual := "vibejson."
	if pkgName == "vibejson" {
		qual = ""
	}

	for _, s := range specs {
		renderStruct(&buf, s, qual)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated code: %w\n%s", err, buf.String())
	}
	return formatted, nil
}

func renderStruct(buf *bytes.Buffer, s structSpec, qual string) {
	fieldVar := "_vibejsonFields" + s.name
	if len(s.fields) > 0 {
		fmt.Fprintf(buf, "var %s = %sMakeFieldSet(", fieldVar, qual)
		for i, f := range s.fields {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(buf, "%q", f.jsonName)
		}
		fmt.Fprintln(buf, ")")
		fmt.Fprintln(buf)
	}

	renderMarshal(buf, s, qual)
	renderUnmarshal(buf, s, qual, fieldVar)
}

func renderMarshal(buf *bytes.Buffer, s structSpec, qual string) {
	fmt.Fprintf(buf, "// MarshalVibeJSON appends %s as compact JSON without reflection or unsafe.\n", s.name)
	fmt.Fprintf(buf, "func (v *%s) MarshalVibeJSON(w %sTrustedAppender) %sTrustedAppender {\n", s.name, qual, qual)
	if len(s.fields) == 0 {
		fmt.Fprintln(buf, `	return w.RawUnchecked("{}")`)
		fmt.Fprintln(buf, "}")
		fmt.Fprintln(buf)
		return
	}

	hasOmit := false
	for _, f := range s.fields {
		if f.omitEmpty {
			hasOmit = true
			break
		}
	}

	if !hasOmit {
		for i, f := range s.fields {
			prefix := ","
			if i == 0 {
				prefix = "{"
			}
			emitFieldKey(buf, prefix, f.jsonName)
			emitMarshalValue(buf, "v."+f.goName, f.typ, 1)
		}
		fmt.Fprintln(buf, "	return w.RawByteUnchecked('}')")
		fmt.Fprintln(buf, "}")
		fmt.Fprintln(buf)
		return
	}

	fmt.Fprintln(buf, "	w = w.RawByteUnchecked('{')")
	fmt.Fprintln(buf, "	first := true")
	for _, f := range s.fields {
		if f.omitEmpty {
			fmt.Fprintf(buf, "	if %s {\n", nonEmptyExpr("v."+f.goName, f.typ))
			fmt.Fprintln(buf, "		if !first {")
			fmt.Fprintln(buf, "			w = w.RawByteUnchecked(',')")
			fmt.Fprintln(buf, "		}")
			fmt.Fprintln(buf, "		first = false")
			emitFieldKey(buf, "", f.jsonName)
			emitMarshalValue(buf, "v."+f.goName, f.typ, 2)
			fmt.Fprintln(buf, "	}")
		} else {
			fmt.Fprintln(buf, "	if !first {")
			fmt.Fprintln(buf, "		w = w.RawByteUnchecked(',')")
			fmt.Fprintln(buf, "	}")
			fmt.Fprintln(buf, "	first = false")
			emitFieldKey(buf, "", f.jsonName)
			emitMarshalValue(buf, "v."+f.goName, f.typ, 1)
		}
	}
	fmt.Fprintln(buf, "	return w.RawByteUnchecked('}')")
	fmt.Fprintln(buf, "}")
	fmt.Fprintln(buf)
}

func emitFieldKey(buf *bytes.Buffer, prefix, jsonName string) {
	escHTML, _ := json.Marshal(jsonName)
	var noHTML bytes.Buffer
	enc := json.NewEncoder(&noHTML)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(jsonName)
	escPlain := bytes.TrimSuffix(noHTML.Bytes(), []byte("\n"))

	if bytes.Equal(escHTML, escPlain) {
		fmt.Fprintf(buf, "	w = w.RawUnchecked(%q)\n", prefix+string(escHTML)+":")
		return
	}
	fmt.Fprintln(buf, "	if w.EscapeHTML() {")
	fmt.Fprintf(buf, "		w = w.RawUnchecked(%q)\n", prefix+string(escHTML)+":")
	fmt.Fprintln(buf, "	} else {")
	fmt.Fprintf(buf, "		w = w.RawUnchecked(%q)\n", prefix+string(escPlain)+":")
	fmt.Fprintln(buf, "	}")
}

func nonEmptyExpr(expr string, ft fieldType) string {
	switch ft.kind {
	case kindBool:
		return expr
	case kindString, kindSlice:
		return fmt.Sprintf("len(%s) != 0", expr)
	case kindInt, kindUint, kindFloat32, kindFloat64:
		return fmt.Sprintf("%s != 0", expr)
	case kindPointer:
		return fmt.Sprintf("%s != nil", expr)
	default:
		return "true"
	}
}

func emitMarshalValue(buf *bytes.Buffer, expr string, ft fieldType, depth int) {
	ind := strings.Repeat("\t", depth)
	switch ft.kind {
	case kindBool:
		fmt.Fprintf(buf, "%sw = w.Bool(%s)\n", ind, expr)
	case kindString:
		fmt.Fprintf(buf, "%sw = w.String(%s)\n", ind, expr)
	case kindInt:
		fmt.Fprintf(buf, "%sw = w.Int(int64(%s))\n", ind, expr)
	case kindUint:
		fmt.Fprintf(buf, "%sw = w.Uint(uint64(%s))\n", ind, expr)
	case kindFloat32:
		fmt.Fprintf(buf, "%sw = w.Float32(%s)\n", ind, expr)
	case kindFloat64:
		fmt.Fprintf(buf, "%sw = w.Float64(%s)\n", ind, expr)
	case kindStruct:
		fmt.Fprintf(buf, "%sw = (%s).MarshalVibeJSON(w)\n", ind, expr)
	case kindPointer:
		fmt.Fprintf(buf, "%sif %s == nil {\n", ind, expr)
		fmt.Fprintf(buf, "%s\tw = w.Null()\n", ind)
		fmt.Fprintf(buf, "%s} else {\n", ind)
		if ft.elem.kind == kindStruct {
			fmt.Fprintf(buf, "%s\tw = (%s).MarshalVibeJSON(w)\n", ind, expr)
		} else {
			emitMarshalValue(buf, "*("+expr+")", *ft.elem, depth+1)
		}
		fmt.Fprintf(buf, "%s}\n", ind)
	case kindSlice:
		idx := fmt.Sprintf("i%d", depth)
		fmt.Fprintf(buf, "%sif %s == nil {\n", ind, expr)
		fmt.Fprintf(buf, "%s\tw = w.Null()\n", ind)
		fmt.Fprintf(buf, "%s} else {\n", ind)
		fmt.Fprintf(buf, "%s\tw = w.RawByteUnchecked('[')\n", ind)
		fmt.Fprintf(buf, "%s\tfor %s := range %s {\n", ind, idx, expr)
		fmt.Fprintf(buf, "%s\t\tif %s > 0 {\n", ind, idx)
		fmt.Fprintf(buf, "%s\t\t\tw = w.RawByteUnchecked(',')\n", ind)
		fmt.Fprintf(buf, "%s\t\t}\n", ind)
		emitMarshalValue(buf, fmt.Sprintf("%s[%s]", expr, idx), *ft.elem, depth+2)
		fmt.Fprintf(buf, "%s\t}\n", ind)
		fmt.Fprintf(buf, "%s\tw = w.RawByteUnchecked(']')\n", ind)
		fmt.Fprintf(buf, "%s}\n", ind)
	case kindArray:
		idx := fmt.Sprintf("i%d", depth)
		fmt.Fprintf(buf, "%sw = w.RawByteUnchecked('[')\n", ind)
		fmt.Fprintf(buf, "%sfor %s := range %s {\n", ind, idx, expr)
		fmt.Fprintf(buf, "%s\tif %s > 0 {\n", ind, idx)
		fmt.Fprintf(buf, "%s\t\tw = w.RawByteUnchecked(',')\n", ind)
		fmt.Fprintf(buf, "%s\t}\n", ind)
		emitMarshalValue(buf, fmt.Sprintf("%s[%s]", expr, idx), *ft.elem, depth+1)
		fmt.Fprintf(buf, "%s}\n", ind)
		fmt.Fprintf(buf, "%sw = w.RawByteUnchecked(']')\n", ind)
	}
}

func renderUnmarshal(buf *bytes.Buffer, s structSpec, qual, fieldVar string) {
	fmt.Fprintf(buf, "// UnmarshalVibeJSON decodes %s directly without reflection or unsafe.\n", s.name)
	fmt.Fprintf(buf, "func (v *%s) UnmarshalVibeJSON(c %sDecodeCursor) (%sDecodeCursor, error) {\n", s.name, qual, qual)
	fmt.Fprintln(buf, "	if null, err := c.Null(); err != nil {")
	fmt.Fprintln(buf, "		return c, err")
	fmt.Fprintln(buf, "	} else if null {")
	fmt.Fprintln(buf, "		return c, nil")
	fmt.Fprintln(buf, "	}")
	fmt.Fprintf(buf, "	if err := c.BeginObject(%q); err != nil {\n", s.name)
	fmt.Fprintln(buf, "		return c, err")
	fmt.Fprintln(buf, "	}")

	if len(s.fields) == 0 {
		fmt.Fprintln(buf, "	if c.ExpectObjectClose() {")
		fmt.Fprintln(buf, "		return c, nil")
		fmt.Fprintln(buf, "	}")
		fmt.Fprintln(buf, "	err := v.unmarshalVibeJSONRest(&c, true)")
		fmt.Fprintln(buf, "	return c, err")
		fmt.Fprintln(buf, "}")
		fmt.Fprintln(buf)
	} else {
		for i, f := range s.fields {
			firstArg := "false"
			if i == 0 {
				firstArg = "true"
			}
			ind := strings.Repeat("\t", i+1)
			fmt.Fprintf(buf, "%sif c.Field(%s, %s.Field(%d)) {\n", ind, firstArg, fieldVar, i)
			emitDecodeInto(buf, "&v."+f.goName, f.typ, "c", i+2, true)
		}
		leafInd := strings.Repeat("\t", len(s.fields)+1)
		fmt.Fprintf(buf, "%sif c.ExpectObjectClose() {\n", leafInd)
		fmt.Fprintf(buf, "%s\treturn c, nil\n", leafInd)
		fmt.Fprintf(buf, "%s}\n", leafInd)
		fmt.Fprintf(buf, "%serr := v.unmarshalVibeJSONRest(&c, false)\n", leafInd)
		fmt.Fprintf(buf, "%sreturn c, err\n", leafInd)
		for i := len(s.fields) - 1; i >= 0; i-- {
			ind := strings.Repeat("\t", i+1)
			fmt.Fprintf(buf, "%s}\n", ind)
			if i > 0 {
				fmt.Fprintf(buf, "%serr := v.unmarshalVibeJSONRest(&c, false)\n", ind)
				fmt.Fprintf(buf, "%sreturn c, err\n", ind)
			} else {
				fmt.Fprintf(buf, "%serr := v.unmarshalVibeJSONRest(&c, true)\n", ind)
				fmt.Fprintf(buf, "%sreturn c, err\n", ind)
			}
		}
		fmt.Fprintln(buf, "}")
		fmt.Fprintln(buf)
	}

	fmt.Fprintf(buf, "func (v *%s) unmarshalVibeJSONRest(c *%sDecodeCursor, first bool) error {\n", s.name, qual)
	if len(s.fields) > 0 {
		fmt.Fprintln(buf, "	cs := c.CaseSensitive()")
	}
	fmt.Fprintln(buf, "	for {")
	fmt.Fprintln(buf, "		key, ok, err := c.NextField(first)")
	fmt.Fprintln(buf, "		if err != nil {")
	fmt.Fprintln(buf, "			return err")
	fmt.Fprintln(buf, "		}")
	fmt.Fprintln(buf, "		if !ok {")
	fmt.Fprintln(buf, "			return nil")
	fmt.Fprintln(buf, "		}")
	fmt.Fprintln(buf, "		first = false")
	if len(s.fields) == 0 {
		fmt.Fprintln(buf, "		_ = key")
		fmt.Fprintln(buf, "		if err := c.Skip(); err != nil {")
		fmt.Fprintln(buf, "			return err")
		fmt.Fprintln(buf, "		}")
		fmt.Fprintln(buf, "	}")
		fmt.Fprintln(buf, "}")
		fmt.Fprintln(buf)
		return
	}
	fmt.Fprintf(buf, "		idx, known := %s.Lookup(key, cs)\n", fieldVar)
	fmt.Fprintln(buf, "		if !known {")
	fmt.Fprintln(buf, "			if err := c.Skip(); err != nil {")
	fmt.Fprintln(buf, "				return err")
	fmt.Fprintln(buf, "			}")
	fmt.Fprintln(buf, "			continue")
	fmt.Fprintln(buf, "		}")
	fmt.Fprintln(buf, "		switch idx {")
	for i, f := range s.fields {
		fmt.Fprintf(buf, "		case %d:\n", i)
		emitDecodeInto(buf, "&v."+f.goName, f.typ, "*c", 3, false)
	}
	fmt.Fprintln(buf, "		}")
	fmt.Fprintln(buf, "	}")
	fmt.Fprintln(buf, "}")
	fmt.Fprintln(buf)
}

func derefExpr(ptrExpr string) string {
	if strings.HasPrefix(ptrExpr, "&") {
		return ptrExpr[1:]
	}
	return "*(" + ptrExpr + ")"
}

func emitDecodeInto(buf *bytes.Buffer, dstPtrExpr string, ft fieldType, curExpr string, depth int, returnCursor bool) {
	ind := strings.Repeat("\t", depth)
	retErr := func(errVar string) string {
		if returnCursor {
			return fmt.Sprintf("return c, %s", errVar)
		}
		return fmt.Sprintf("return %s", errVar)
	}
	cDot := "c."
	valExpr := derefExpr(dstPtrExpr)

	switch ft.kind {
	case kindBool:
		fmt.Fprintf(buf, "%sif err := %sBool(%s); err != nil {\n%s\t%s\n%s}\n", ind, cDot, dstPtrExpr, ind, retErr("err"), ind)
	case kindString:
		fmt.Fprintf(buf, "%sif err := %sString(%s); err != nil {\n%s\t%s\n%s}\n", ind, cDot, dstPtrExpr, ind, retErr("err"), ind)
	case kindInt:
		fmt.Fprintf(buf, "%sif err := %sInt(%s); err != nil {\n%s\t%s\n%s}\n", ind, cDot, dstPtrExpr, ind, retErr("err"), ind)
	case kindUint:
		fmt.Fprintf(buf, "%sif err := %sUint(%s); err != nil {\n%s\t%s\n%s}\n", ind, cDot, dstPtrExpr, ind, retErr("err"), ind)
	case kindFloat32, kindFloat64:
		fmt.Fprintf(buf, "%sif err := %sFloat(%s); err != nil {\n%s\t%s\n%s}\n", ind, cDot, dstPtrExpr, ind, retErr("err"), ind)
	case kindStruct:
		fmt.Fprintf(buf, "%s{\n", ind)
		fmt.Fprintf(buf, "%s\tnext, err := (%s).UnmarshalVibeJSON(%s)\n", ind, dstPtrExpr, curExpr)
		fmt.Fprintf(buf, "%s\t%s = next\n", ind, curExpr)
		fmt.Fprintf(buf, "%s\tif err != nil {\n%s\t\t%s\n%s\t}\n", ind, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s}\n", ind)
	case kindPointer:
		pVar := fmt.Sprintf("p%d", depth)
		fmt.Fprintf(buf, "%sif null, err := %sNull(); err != nil {\n%s\t%s\n%s} else if null {\n", ind, cDot, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t%s = nil\n", ind, valExpr)
		fmt.Fprintf(buf, "%s} else {\n", ind)
		fmt.Fprintf(buf, "%s\t%s := %s\n", ind, pVar, valExpr)
		fmt.Fprintf(buf, "%s\tif %s == nil {\n", ind, pVar)
		fmt.Fprintf(buf, "%s\t\t%s = new(%s)\n", ind, pVar, ft.elem.typeName)
		fmt.Fprintf(buf, "%s\t\t%s = %s\n", ind, valExpr, pVar)
		fmt.Fprintf(buf, "%s\t}\n", ind)
		emitDecodeInto(buf, pVar, *ft.elem, curExpr, depth+1, returnCursor)
		fmt.Fprintf(buf, "%s}\n", ind)
	case kindSlice:
		sVar := fmt.Sprintf("s%d", depth)
		eVar := fmt.Sprintf("e%d", depth)
		fVar := fmt.Sprintf("first%d", depth)
		fmt.Fprintf(buf, "%sif null, err := %sNull(); err != nil {\n%s\t%s\n%s} else if null {\n", ind, cDot, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t%s = nil\n", ind, valExpr)
		fmt.Fprintf(buf, "%s} else {\n", ind)
		fmt.Fprintf(buf, "%s\tif err := %sBeginArray(%q); err != nil {\n%s\t\t%s\n%s\t}\n", ind, cDot, ft.typeName, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t%s := (%s)[:0]\n", ind, sVar, valExpr)
		fmt.Fprintf(buf, "%s\tif %s == nil {\n%s\t\t%s = make(%s, 0)\n%s\t}\n", ind, sVar, ind, sVar, ft.typeName, ind)
		fmt.Fprintf(buf, "%s\tfor %s := true; ; %s = false {\n", ind, fVar, fVar)
		fmt.Fprintf(buf, "%s\t\tmore, err := %sNextElement(%s)\n", ind, cDot, fVar)
		fmt.Fprintf(buf, "%s\t\tif err != nil {\n%s\t\t\t%s = %s\n%s\t\t\t%s\n%s\t\t}\n", ind, ind, valExpr, sVar, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t\tif !more {\n%s\t\t\tbreak\n%s\t\t}\n", ind, ind, ind)
		fmt.Fprintf(buf, "%s\t\tvar %s %s\n", ind, eVar, ft.elem.typeName)
		emitDecodeInto(buf, "&"+eVar, *ft.elem, curExpr, depth+2, returnCursor)
		fmt.Fprintf(buf, "%s\t\t%s = append(%s, %s)\n", ind, sVar, sVar, eVar)
		fmt.Fprintf(buf, "%s\t}\n", ind)
		fmt.Fprintf(buf, "%s\t%s = %s\n", ind, valExpr, sVar)
		fmt.Fprintf(buf, "%s}\n", ind)
	case kindArray:
		idxVar := fmt.Sprintf("idx%d", depth)
		aVar := fmt.Sprintf("a%d", depth)
		fmt.Fprintf(buf, "%sif null, err := %sNull(); err != nil {\n%s\t%s\n%s} else if !null {\n", ind, cDot, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\tif err := %sBeginArray(%q); err != nil {\n%s\t\t%s\n%s\t}\n", ind, cDot, ft.typeName, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t%s := %s\n", ind, aVar, dstPtrExpr)
		fmt.Fprintf(buf, "%s\t%s := 0\n", ind, idxVar)
		fmt.Fprintf(buf, "%s\tfor {\n", ind)
		fmt.Fprintf(buf, "%s\t\tmore, err := %sNextElement(%s == 0)\n", ind, cDot, idxVar)
		fmt.Fprintf(buf, "%s\t\tif err != nil {\n%s\t\t\t%s\n%s\t\t}\n", ind, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t\tif !more {\n%s\t\t\tbreak\n%s\t\t}\n", ind, ind, ind)
		fmt.Fprintf(buf, "%s\t\tif %s < len(%s) {\n", ind, idxVar, aVar)
		emitDecodeInto(buf, fmt.Sprintf("&%s[%s]", aVar, idxVar), *ft.elem, curExpr, depth+3, returnCursor)
		fmt.Fprintf(buf, "%s\t\t} else if err := %sSkip(); err != nil {\n%s\t\t\t%s\n%s\t\t}\n", ind, cDot, ind, retErr("err"), ind)
		fmt.Fprintf(buf, "%s\t\t%s++\n", ind, idxVar)
		fmt.Fprintf(buf, "%s\t}\n", ind)
		fmt.Fprintf(buf, "%s\tfor ; %s < len(%s); %s++ {\n", ind, idxVar, aVar, idxVar)
		fmt.Fprintf(buf, "%s\t\tvar zero %s\n", ind, ft.elem.typeName)
		fmt.Fprintf(buf, "%s\t\t%s[%s] = zero\n", ind, aVar, idxVar)
		fmt.Fprintf(buf, "%s\t}\n", ind)
		fmt.Fprintf(buf, "%s}\n", ind)
	}
}
