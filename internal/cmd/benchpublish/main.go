// Command benchpublish validates and normalizes the committed benchmark record
// and renders its small set of checked-in SVG views.
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
	charts, err := renderCharts(absRoot, publication)
	if err != nil {
		fatalf("render charts: %v", err)
	}
	if *write {
		artifacts := make([]artifact, 0, len(charts)+1)
		artifacts = append(artifacts, artifact{path: resultPath, data: data})
		artifacts = append(artifacts, charts...)
		if err := writeArtifacts(artifacts); err != nil {
			fatalf("write publication: %v", err)
		}
		return
	}
	if !bytes.Equal(committed, data) {
		fatalf("%s is not normalized", resultPath)
	}
	for _, chart := range charts {
		got, readErr := os.ReadFile(chart.path)
		if readErr != nil {
			fatalf("read %s: %v", chart.path, readErr)
		}
		if !bytes.Equal(got, chart.data) {
			fatalf("%s is stale", chart.path)
		}
	}
}

type artifact struct {
	path string
	data []byte
}

func writeArtifacts(artifacts []artifact) error {
	for _, artifact := range artifacts {
		if err := os.MkdirAll(filepath.Dir(artifact.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(artifact.path, artifact.data, 0o644); err != nil {
			return err
		}
	}
	return nil
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
