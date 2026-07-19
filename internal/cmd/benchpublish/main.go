// Command benchpublish validates and normalizes the committed benchmark record.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	var (
		root      = flag.String("root", ".", "repository root")
		input     = flag.String("input", "", "directory containing main.txt, hooks.txt, and crosslang.txt")
		count     = flag.Int("count", 6, "samples per Go benchmark")
		benchtime = flag.String("benchtime", "300ms", "time per Go benchmark sample")
		write     = flag.Bool("write", false, "write the normalized result record")
		check     = flag.Bool("check", false, "validate the committed result record")
	)
	flag.Parse()
	if boolCount(*write, *check) != 1 {
		fatalf("select exactly one of -write or -check")
	}
	if *write && *input == "" {
		fatalf("-write requires -input")
	}
	if *check && *input != "" {
		fatalf("-check reads the committed record; omit -input")
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatalf("resolve repository root: %v", err)
	}
	resultPath := filepath.Join(absRoot, "benchmarks", "results", "latest.json")

	var (
		publication Publication
		committed   []byte
	)
	if *input != "" {
		publication, err = parsePublication(*input, *count, *benchtime)
		if err != nil {
			fatalf("parse publication: %v", err)
		}
	} else {
		committed, err = os.ReadFile(resultPath)
		if err != nil {
			fatalf("read %s: %v", resultPath, err)
		}
		if err = json.Unmarshal(committed, &publication); err != nil {
			fatalf("decode %s: %v", resultPath, err)
		}
	}
	if err := publication.validate(); err != nil {
		fatalf("invalid publication: %v", err)
	}
	data, err := encodePublication(publication)
	if err != nil {
		fatalf("encode results: %v", err)
	}
	if *write {
		if err := os.WriteFile(resultPath, data, 0o644); err != nil {
			fatalf("write normalized results: %v", err)
		}
		return
	}
	if !bytes.Equal(committed, data) {
		fatalf("%s is not normalized", resultPath)
	}
}

func encodePublication(publication Publication) ([]byte, error) {
	data, err := json.MarshalIndent(publication, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "benchpublish: "+format+"\n", args...)
	os.Exit(1)
}
