//go:build ignore

// decoder_cursor_gen renders the compiler-specific decoder cursor sources.
// It deliberately treats the Go source as preformatted text: stable Go cannot
// parse Go 1.27 generic methods, but it must still be able to reproduce every
// generated file byte-for-byte.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"
)

type generatedFile struct {
	Path     string
	Output   string
	BuildTag string
	Go127    bool
}

var generatedFiles = []generatedFile{
	{
		Path:     "decoder_cursor_pre_go127.go",
		Output:   "cursor",
		BuildTag: "!go1.27",
	},
	{
		Path:     "decoder_cursor_go127.go",
		Output:   "cursor",
		BuildTag: "go1.27",
		Go127:    true,
	},
	{
		Path:     "decoder_numeric_methods_pre_go127.go",
		Output:   "numeric",
		BuildTag: "!go1.27",
	},
	{
		Path:     "decoder_numeric_methods_go127.go",
		Output:   "numeric",
		BuildTag: "go1.27",
		Go127:    true,
	},
}

func main() {
	source, err := os.ReadFile("decoder_cursor.tmpl")
	if err != nil {
		fail(err)
	}
	// Control actions live on their own lines so the template remains easy to
	// read. Remove only those physical newlines before rendering; otherwise a
	// disabled branch would leave blank or indented lines in generated Go.
	source = compactControlLines(source)
	tmpl, err := template.New("decoder_cursor").Option("missingkey=error").Parse(string(source))
	if err != nil {
		fail(err)
	}

	type renderedFile struct {
		path string
		data []byte
	}
	rendered := make([]renderedFile, 0, len(generatedFiles))
	for _, file := range generatedFiles {
		var output bytes.Buffer
		if err := tmpl.Execute(&output, file); err != nil {
			fail(err)
		}
		rendered = append(rendered, renderedFile{path: file.Path, data: output.Bytes()})
	}

	for _, file := range rendered {
		current, err := os.ReadFile(file.path)
		if err == nil && bytes.Equal(current, file.data) {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			fail(err)
		}
		if err := os.WriteFile(file.path, file.data, 0o644); err != nil {
			fail(err)
		}
	}
}

func compactControlLines(source []byte) []byte {
	lines := strings.SplitAfter(string(source), "\n")
	var compact strings.Builder
	compact.Grow(len(source))
	for _, line := range lines {
		action := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if strings.HasPrefix(action, "{{if ") ||
			strings.HasPrefix(action, "{{else") ||
			strings.HasPrefix(action, "{{end") {
			action = strings.ReplaceAll(action, " -}}", "}}")
			compact.WriteString(action)
			continue
		}
		compact.WriteString(line)
	}
	return []byte(compact.String())
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "decoder_cursor_gen:", err)
	os.Exit(1)
}
