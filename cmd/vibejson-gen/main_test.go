package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestGenerateStructCodecsZeroUnsafe(t *testing.T) {
	const src = `package sample

type Address struct {
	Street string ` + "`json:\"street\"`" + `
	Zip    int    ` + "`json:\"zip,omitempty\"`" + `
}

type Order struct {
	ID       int64     ` + "`json:\"id\"`" + `
	Active   bool      ` + "`json:\"active\"`" + `
	Customer string    ` + "`json:\"customer\"`" + `
	Note     string    ` + "`json:\"note,omitempty\"`" + `
	Scores   [3]float64 ` + "`json:\"scores\"`" + `
	Tags     []string  ` + "`json:\"tags,omitempty\"`" + `
	Shipping *Address  ` + "`json:\"shipping,omitempty\"`" + `
	Billing  Address   ` + "`json:\"billing\"`" + `
	Ignored  string    ` + "`json:\"-\"`" + `
	private  int
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateFiles("sample", []*ast.File{file}, []string{"Order", "Address"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)

	if strings.Contains(text, `"unsafe"`) || strings.Contains(text, `"reflect"`) {
		t.Fatalf("generated code must not import unsafe or reflect:\n%s", text)
	}
	for _, want := range []string{
		"func (v *Order) MarshalVibeJSON(w vibejson.TrustedAppender) vibejson.TrustedAppender",
		"func (v *Order) UnmarshalVibeJSON(c vibejson.DecodeCursor) (vibejson.DecodeCursor, error)",
		"func (v *Address) MarshalVibeJSON(w vibejson.TrustedAppender) vibejson.TrustedAppender",
		"func (v *Address) UnmarshalVibeJSON(c vibejson.DecodeCursor) (vibejson.DecodeCursor, error)",
		`vibejson.MakeFieldSet("id", "active", "customer", "note", "scores", "tags", "shipping", "billing")`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated code missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Ignored") || strings.Contains(text, "private") {
		t.Fatalf("generated code should omit ignored and unexported fields:\n%s", text)
	}
	// Verify generated output parses cleanly as Go.
	if _, err := parser.ParseFile(token.NewFileSet(), "order_vibejson.go", out, 0); err != nil {
		t.Fatalf("generated code failed to parse: %v\n%s", err, text)
	}
}

func TestGenerateHTMLFieldNameEscaping(t *testing.T) {
	const src = `package sample

type Widget struct {
	Tag string ` + "`json:\"a<b>&c\"`" + `
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "widget.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateFiles("sample", []*ast.File{file}, []string{"Widget"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "w.EscapeHTML()") {
		t.Fatalf("expected HTML escaping branch for field name with <>&:\n%s", text)
	}
}
