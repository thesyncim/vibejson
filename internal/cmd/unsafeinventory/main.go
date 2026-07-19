// Command unsafeinventory checks the repository's unsafe-code inventory.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	beginMarker = "<!-- BEGIN GENERATED UNSAFE SCOPES -->"
	endMarker   = "<!-- END GENERATED UNSAFE SCOPES -->"
)

type unsafeScope struct {
	file string
	name string
}

func main() {
	checkPath := flag.String("check", "", "check the generated block in file")
	writePath := flag.String("write", "", "replace the generated block in file")
	flag.Parse()
	if flag.NArg() != 0 || (*checkPath != "" && *writePath != "") {
		flag.Usage()
		os.Exit(2)
	}

	scopes, err := findUnsafeScopes(".")
	if err != nil {
		fatal(err)
	}
	block := render(scopes)

	switch {
	case *checkPath != "":
		data, err := os.ReadFile(*checkPath)
		if err != nil {
			fatal(err)
		}
		current, err := generatedBlock(data)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", *checkPath, err))
		}
		if !bytes.Equal(current, block) {
			fatal(fmt.Errorf("%s is stale; run go run ./internal/cmd/unsafeinventory -write %s", *checkPath, *checkPath))
		}
	case *writePath != "":
		data, err := os.ReadFile(*writePath)
		if err != nil {
			fatal(err)
		}
		current, err := generatedBlock(data)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", *writePath, err))
		}
		data = bytes.Replace(data, current, block, 1)
		if err := os.WriteFile(*writePath, data, 0o644); err != nil {
			fatal(err)
		}
	default:
		_, _ = os.Stdout.Write(block)
	}
}

func findUnsafeScopes(root string) ([]unsafeScope, error) {
	var scopes []unsafeScope
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fileScopes, err := scanFile(root, path)
		if err != nil {
			return err
		}
		scopes = append(scopes, fileScopes...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(scopes, func(a, b unsafeScope) int {
		if n := strings.Compare(a.file, b.file); n != 0 {
			return n
		}
		return strings.Compare(a.name, b.name)
	})
	return slices.Compact(scopes), nil
}

func scanFile(root, path string) ([]unsafeScope, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			directive := forbiddenCompilerDirective(comment.Text)
			if directive == "" {
				continue
			}
			pos := fset.PositionFor(comment.Slash, false)
			return nil, fmt.Errorf("%s: forbidden compiler directive %s", pos, directive)
		}
	}

	aliases := make(map[string]bool)
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if importPath != "unsafe" {
			continue
		}
		if spec.Name != nil {
			pos := fset.PositionFor(spec.Name.Pos(), false)
			return nil, fmt.Errorf("%s: unsafe import must use the canonical name", pos)
		}
		aliases["unsafe"] = true
	}
	if len(aliases) == 0 {
		return nil, nil
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	rel = filepath.ToSlash(rel)
	var scopes []unsafeScope
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if containsUnsafe(decl, aliases) {
				name, err := functionName(fset, decl)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
				scopes = append(scopes, unsafeScope{file: rel, name: name})
			}
		case *ast.GenDecl:
			if containsUnsafe(decl, aliases) {
				scopes = append(scopes, unsafeScope{file: rel, name: "package scope"})
			}
		}
	}
	return scopes, nil
}

func forbiddenCompilerDirective(text string) string {
	switch {
	case text == "//go:noescape", strings.HasPrefix(text, "//go:noescape "):
		return "//go:noescape"
	case strings.HasPrefix(text, "//go:linkname "):
		return "//go:linkname"
	case strings.HasPrefix(text, "//go:linknamestd "):
		return "//go:linknamestd"
	default:
		return ""
	}
}

func containsUnsafe(node ast.Node, aliases map[string]bool) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return !found
		}
		ident, ok := selector.X.(*ast.Ident)
		if ok && aliases[ident.Name] {
			found = true
		}
		return !found
	})
	return found
}

func functionName(fset *token.FileSet, decl *ast.FuncDecl) (string, error) {
	if decl.Recv == nil {
		return decl.Name.Name, nil
	}
	var receiver bytes.Buffer
	if err := format.Node(&receiver, fset, decl.Recv.List[0].Type); err != nil {
		return "", err
	}
	return "(" + receiver.String() + ")." + decl.Name.Name, nil
}

func render(scopes []unsafeScope) []byte {
	var out bytes.Buffer
	fmt.Fprintln(&out, beginMarker)
	fmt.Fprintln(&out, "<!-- Generated by internal/cmd/unsafeinventory; do not edit this block. -->")
	for _, scope := range scopes {
		fmt.Fprintf(&out, "- `%s` — `%s`\n", scope.file, scope.name)
	}
	fmt.Fprintln(&out, endMarker)
	return out.Bytes()
}

func generatedBlock(data []byte) ([]byte, error) {
	start := bytes.Index(data, []byte(beginMarker))
	if start < 0 {
		return nil, fmt.Errorf("missing %s", beginMarker)
	}
	end := bytes.Index(data[start:], []byte(endMarker))
	if end < 0 {
		return nil, fmt.Errorf("missing %s", endMarker)
	}
	end += start + len(endMarker)
	if end < len(data) && data[end] == '\n' {
		end++
	}
	return data[start:end], nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "unsafeinventory:", err)
	os.Exit(1)
}
