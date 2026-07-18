//go:build ignore

// typed_ops_gen generates the repeated typed-operation dispatch matrices.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"strings"
)

type operation struct {
	name       string
	kind       string
	goType     string
	decode     bool
	encode     bool
	structural bool
}

var operations = []operation{
	{name: "Invalid", kind: "invalid"},
	{name: "Bool", kind: "bool", goType: "bool", decode: true, encode: true, structural: true},
	{name: "String", kind: "string", goType: "string", decode: true, encode: true, structural: true},
	{name: "Number", kind: "number", goType: "string", decode: true, encode: true},
	{name: "Int8", kind: "int", goType: "int8", decode: true, encode: true},
	{name: "Int16", kind: "int", goType: "int16", decode: true, encode: true},
	{name: "Int32", kind: "int", goType: "int32", decode: true, encode: true},
	{name: "Int64", kind: "int", goType: "int64", decode: true, encode: true, structural: true},
	{name: "Uint8", kind: "uint", goType: "uint8", decode: true, encode: true},
	{name: "Uint16", kind: "uint", goType: "uint16", decode: true, encode: true},
	{name: "Uint32", kind: "uint", goType: "uint32", decode: true, encode: true},
	{name: "Uint64", kind: "uint", goType: "uint64", decode: true, encode: true},
	{name: "Float32", kind: "float32", goType: "float32", decode: true, encode: true},
	{name: "Float64", kind: "float64", goType: "float64", decode: true, encode: true, structural: true},
	{name: "Struct", kind: "struct", decode: true, encode: true, structural: true},
	{name: "Slice", kind: "slice", decode: true, encode: true, structural: true},
	{name: "Array", kind: "array", decode: true, encode: true, structural: true},
	{name: "Pointer", kind: "pointer", decode: true},
	{name: "Map", kind: "map", decode: true, encode: true},
	{name: "Any", kind: "any", decode: true, encode: true},
	{name: "Bytes", kind: "bytes", decode: true},
	{name: "Quoted", kind: "quoted", decode: true, encode: true},
	{name: "Unmarshaler", kind: "unmarshaler", decode: true},
	{name: "Marshaler", kind: "marshaler", encode: true},
	{name: "Iface", kind: "iface", decode: true},
}

func main() {
	rewrites := []struct {
		path string
		name string
		body string
	}{
		{"typed.go", "TYPED OP ENUM", renderEnum()},
		{"typed.go", "TYPED STRUCTURAL FIELD ELIGIBILITY", renderStructuralEligibility()},
		{"typed_compiled_record.go", "TYPED CURSOR FIELD DISPATCH", renderDecode(false)},
		{"typed_compiled_record.go", "TYPED STRUCTURAL FIELD DISPATCH", renderDecode(true)},
		{"encoder_execute_record.go", "TYPED ENCODER FIELD DISPATCH", renderEncodeField()},
		{"encoder_execute_record_specialized.go", "TYPED ENCODER VALUE DISPATCH", renderEncodeValue()},
	}
	for _, rewrite := range rewrites {
		if err := replaceGeneratedBlock(rewrite.path, rewrite.name, rewrite.body); err != nil {
			fmt.Fprintln(os.Stderr, "typed_ops_gen:", err)
			os.Exit(1)
		}
	}
}

func renderEnum() string {
	var out strings.Builder
	for i, op := range operations {
		fmt.Fprintf(&out, "\ttypedOp%s", op.name)
		if i == 0 {
			out.WriteString(" typedOp = iota")
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func renderStructuralEligibility() string {
	var names []string
	for _, op := range operations {
		if op.structural {
			names = append(names, "typedOp"+op.name)
		}
	}
	return "\t\t\tcase " + strings.Join(names, ", ") + ":\n"
}

func renderDecode(structural bool) string {
	var out strings.Builder
	for _, op := range operations {
		if !op.decode {
			continue
		}
		fmt.Fprintf(&out, "\t\tcase typedOp%s:\n", op.name)
		for _, line := range decodeBody(op, structural) {
			fmt.Fprintf(&out, "\t\t\t%s\n", line)
		}
	}
	return out.String()
}

func decodeBody(op operation, structural bool) []string {
	switch op.kind {
	case "bool":
		return []string{"fieldErr = cursor.Bool((*bool)(fieldDst))"}
	case "string":
		if structural {
			return []string{"fieldErr = cursor.stringStructural((*string)(fieldDst))"}
		}
		return []string{"fieldErr = cursor.String((*string)(fieldDst))"}
	case "number":
		return []string{"fieldErr = cursor.Number((*string)(fieldDst))"}
	case "int":
		return []string{
			"if useStableNumericMethods {",
			fmt.Sprintf("\tfieldErr = cursor.%s((*%s)(fieldDst))", op.name, op.goType),
			"} else {",
			fmt.Sprintf("\tfieldErr = cursor.Int((*%s)(fieldDst))", op.goType),
			"}",
		}
	case "uint":
		return []string{
			"if useStableNumericMethods {",
			fmt.Sprintf("\tfieldErr = cursor.%s((*%s)(fieldDst))", op.name, op.goType),
			"} else {",
			fmt.Sprintf("\tfieldErr = cursor.Uint((*%s)(fieldDst))", op.goType),
			"}",
		}
	case "float32", "float64":
		return []string{
			"if useStableNumericMethods {",
			fmt.Sprintf("\tfieldErr = cursor.%s((*%s)(fieldDst))", op.name, op.goType),
			"} else {",
			fmt.Sprintf("\tfieldErr = cursor.Float((*%s)(fieldDst))", op.goType),
			"}",
		}
	case "struct":
		method := "decodeCompiledStruct"
		if structural {
			method += "Structural"
		}
		return []string{fmt.Sprintf("fieldErr = cursor.%s(fieldNode, fieldDst)", method)}
	case "slice":
		method := "decodeCompiledSlice"
		if structural {
			method += "Structural"
		}
		return []string{fmt.Sprintf("fieldErr = cursor.%s(fieldNode, fieldDst)", method)}
	case "array":
		method := "decodeCompiledArray"
		if structural {
			method += "Structural"
		}
		return []string{fmt.Sprintf("fieldErr = cursor.%s(fieldNode, fieldDst)", method)}
	case "pointer":
		return []string{"fieldErr = cursor.decodeCompiledPointer(fieldNode, fieldDst)"}
	case "map":
		return []string{"fieldErr = cursor.decodeCompiledMap(fieldNode, fieldDst)"}
	case "any":
		return []string{"fieldErr = cursor.decodeCompiledAny(fieldDst)"}
	case "bytes":
		return []string{"fieldErr = cursor.decodeCompiledBytes(fieldNode, fieldDst)"}
	case "quoted":
		return []string{"fieldErr = cursor.decodeQuotedField(fieldNode, fieldDst)"}
	case "unmarshaler":
		return []string{
			"switch fieldNode.kind {",
			"case typedUnmarshalerJSON:",
			"\tfieldErr = cursor.decodeViaUnmarshaler(fieldNode, fieldDst)",
			"case typedUnmarshalerSimd:",
			"\tfieldErr = cursor.decodeViaSimdHook(fieldNode, fieldDst)",
			"default:",
			"\tfieldErr = cursor.decodeViaTextUnmarshaler(fieldNode, fieldDst)",
			"}",
		}
	case "iface":
		return []string{"fieldErr = cursor.decodeCompiledIface(fieldNode, fieldDst)"}
	default:
		panic("missing decode body for " + op.name)
	}
}

func renderEncodeField() string {
	var out strings.Builder
	for _, op := range operations {
		if !op.encode {
			continue
		}
		fmt.Fprintf(&out, "\t\tcase typedOp%s:\n", op.name)
		for _, line := range encodeFieldBody(op) {
			fmt.Fprintf(&out, "\t\t\t%s\n", line)
		}
	}
	return out.String()
}

func encodeFieldBody(op operation) []string {
	switch op.kind {
	case "bool":
		return []string{
			"if *(*bool)(fieldSrc) {",
			"\te.dst = append(e.dst, \"true\"...)",
			"} else {",
			"\te.dst = append(e.dst, \"false\"...)",
			"}",
		}
	case "string":
		return []string{"e.dst = appendEncodedJSONString(e.dst, *(*string)(fieldSrc), e.escapeHTML)"}
	case "number":
		return []string{"err = e.encodeNumberLiteral(*(*string)(fieldSrc))"}
	case "int":
		expr := fmt.Sprintf("int64(*(*%s)(fieldSrc))", op.goType)
		if op.goType == "int64" {
			expr = "*(*int64)(fieldSrc)"
		}
		return []string{"e.dst = appendCompactInt(e.dst, " + expr + ")"}
	case "uint":
		expr := fmt.Sprintf("uint64(*(*%s)(fieldSrc))", op.goType)
		if op.goType == "uint64" {
			expr = "*(*uint64)(fieldSrc)"
		}
		return []string{"e.dst = appendCompactUint(e.dst, " + expr + ")"}
	case "float32":
		return []string{"err = e.encodeFloat(float64(*(*float32)(fieldSrc)), 32)"}
	case "float64":
		return []string{"err = e.encodeFloat(*(*float64)(fieldSrc), 64)"}
	case "struct", "slice", "array", "map":
		return []string{fmt.Sprintf("err = e.encode%s(encField.node, fieldSrc)", op.name)}
	case "any":
		return []string{"err = e.encodeAny(fieldSrc)"}
	case "quoted":
		return []string{"err = e.encodeQuoted(encField.node, fieldSrc)"}
	case "marshaler":
		return []string{"err = e.encodeMarshaler(encField.node, fieldSrc)"}
	default:
		panic("missing encode field body for " + op.name)
	}
}

func renderEncodeValue() string {
	var out strings.Builder
	for _, op := range operations {
		if !op.encode {
			continue
		}
		fmt.Fprintf(&out, "\tcase typedOp%s:\n", op.name)
		for _, line := range encodeValueBody(op) {
			fmt.Fprintf(&out, "\t\t%s\n", line)
		}
	}
	return out.String()
}

func encodeValueBody(op operation) []string {
	switch op.kind {
	case "bool":
		return []string{
			"if *(*bool)(src) {",
			"\te.dst = append(e.dst, \"true\"...)",
			"} else {",
			"\te.dst = append(e.dst, \"false\"...)",
			"}",
		}
	case "string":
		return []string{"e.dst = appendEncodedJSONString(e.dst, *(*string)(src), e.escapeHTML)"}
	case "number":
		return []string{"return e.encodeNumberLiteral(*(*string)(src))"}
	case "int":
		expr := fmt.Sprintf("int64(*(*%s)(src))", op.goType)
		if op.goType == "int64" {
			expr = "*(*int64)(src)"
		}
		return []string{"e.dst = appendCompactInt(e.dst, " + expr + ")"}
	case "uint":
		expr := fmt.Sprintf("uint64(*(*%s)(src))", op.goType)
		if op.goType == "uint64" {
			expr = "*(*uint64)(src)"
		}
		return []string{"e.dst = appendCompactUint(e.dst, " + expr + ")"}
	case "float32":
		return []string{"return e.encodeFloat(float64(*(*float32)(src)), 32)"}
	case "float64":
		return []string{"return e.encodeFloat(*(*float64)(src), 64)"}
	case "struct", "slice", "array", "map":
		return []string{fmt.Sprintf("return e.encode%s(field.node, src)", op.name)}
	case "any":
		return []string{"return e.encodeAny(src)"}
	case "quoted":
		return []string{"return e.encodeQuoted(field.node, src)"}
	case "marshaler":
		return []string{"return e.encodeMarshaler(field.node, src)"}
	default:
		panic("missing encode value body for " + op.name)
	}
}

func replaceGeneratedBlock(path, name, body string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	begin := []byte("// BEGIN GENERATED " + name)
	end := []byte("// END GENERATED " + name)
	if bytes.Count(data, begin) != 1 || bytes.Count(data, end) != 1 {
		return fmt.Errorf("%s: expected one %q block", path, name)
	}
	beginAt := bytes.Index(data, begin)
	bodyAt := bytes.IndexByte(data[beginAt:], '\n')
	if bodyAt < 0 {
		return fmt.Errorf("%s: unterminated begin marker for %q", path, name)
	}
	bodyAt += beginAt + 1
	endAt := bytes.Index(data[bodyAt:], end)
	if endAt < 0 {
		return fmt.Errorf("%s: missing end marker for %q", path, name)
	}
	endAt += bodyAt
	endLineAt := bytes.LastIndexByte(data[:endAt], '\n') + 1

	generated := make([]byte, 0, len(data)+len(body))
	generated = append(generated, data[:bodyAt]...)
	generated = append(generated, body...)
	generated = append(generated, data[endLineAt:]...)
	formatted, err := format.Source(generated)
	if err != nil {
		return fmt.Errorf("%s: format generated source: %w", path, err)
	}
	if bytes.Equal(data, formatted) {
		return nil
	}
	return os.WriteFile(path, formatted, 0o644)
}
