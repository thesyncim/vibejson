package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanFileInventoriesCanonicalUnsafeImport(t *testing.T) {
	_, scopes, err := scanFixture(t, `package fixture

import "unsafe"

func load(p unsafe.Pointer) {}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []unsafeScope{{file: "fixture.go", name: "load"}}
	if !reflect.DeepEqual(scopes, want) {
		t.Fatalf("scopes = %#v, want %#v", scopes, want)
	}
}

func TestScanFileRejectsForbiddenCompilerDirectives(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		line      int
		directive string
	}{
		{
			name: "noescape without unsafe import",
			source: `package fixture
//go:noescape
func hidden()
`,
			line:      2,
			directive: "//go:noescape",
		},
		{
			name: "linkname with blank unsafe import",
			source: `package fixture
import _ "unsafe"
//go:linkname local target
var local int
`,
			line:      3,
			directive: "//go:linkname",
		},
		{
			name: "standard-library linkname variant",
			source: `package fixture
//go:linknamestd local target
var local int
`,
			line:      2,
			directive: "//go:linknamestd",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, _, err := scanFixture(t, test.source)
			want := fmt.Sprintf("%s:%d:1: forbidden compiler directive %s", path, test.line, test.directive)
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func TestScanFileRejectsNonCanonicalUnsafeImports(t *testing.T) {
	for _, name := range []string{"u", "_", "."} {
		t.Run(name, func(t *testing.T) {
			path, _, err := scanFixture(t, "package fixture\nimport "+name+" \"unsafe\"\n")
			want := path + ":2:8: unsafe import must use the canonical name"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func TestForbiddenCompilerDirectiveIgnoresProseAndNearMatches(t *testing.T) {
	for _, text := range []string{
		"// See //go:noescape",
		"/* //go:noescape */",
		"// go:noescape",
		"//go:noescape-like",
		"//go:noescape\texplanation",
		"//go:linkname",
		"//go:linkname-like",
		"//go:linkname\tlocal target",
		"//go:linknamestd",
		"//go:linknamestd-like",
	} {
		t.Run(text, func(t *testing.T) {
			if got := forbiddenCompilerDirective(text); got != "" {
				t.Fatalf("forbiddenCompilerDirective(%q) = %q", text, got)
			}
		})
	}
}

func TestForbiddenCompilerDirectiveMatchesCompilerForms(t *testing.T) {
	tests := map[string]string{
		"//go:noescape":                      "//go:noescape",
		"//go:noescape explanation":          "//go:noescape",
		"//go:linkname local target":         "//go:linkname",
		"//go:linknamestd local target":      "//go:linknamestd",
		"//go:linknamestd local other extra": "//go:linknamestd",
	}
	for text, want := range tests {
		t.Run(text, func(t *testing.T) {
			if got := forbiddenCompilerDirective(text); got != want {
				t.Fatalf("forbiddenCompilerDirective(%q) = %q, want %q", text, got, want)
			}
		})
	}
}

func scanFixture(t *testing.T, source string) (string, []unsafeScope, error) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "fixture.go")
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	scopes, err := scanFile(root, path)
	return path, scopes, err
}
