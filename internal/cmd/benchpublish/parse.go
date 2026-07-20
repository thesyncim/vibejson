package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var cpuSuffix = regexp.MustCompile(`-\d+$`)

func parsePublication(input string, count int, benchtime string) (Publication, error) {
	publication := Publication{Metadata: Metadata{
		Samples:      count,
		BenchTime:    benchtime,
		CXXLibrary:   "simdjson 4.6.4",
		CXXCommit:    "1bcf71bd85059ab6574ea1159de9298dcc1212c5",
		GoExperiment: "nosimd,simd",
	}}
	byKey := make(map[string]*BenchmarkResult)
	for _, file := range []struct {
		name    string
		variant string
	}{
		{name: "main.txt"},
		{name: "hooks.txt", variant: "hooks"},
	} {
		if err := parseBenchmarkFile(filepath.Join(input, file.name), file.variant, &publication.Metadata, byKey); err != nil {
			return Publication{}, err
		}
	}
	crosslang, err := parseCrosslangFile(filepath.Join(input, "crosslang.txt"), &publication.Metadata)
	if err != nil {
		return Publication{}, err
	}
	publication.Crosslang = crosslang
	for _, result := range byKey {
		if len(result.NsPerOp) != count {
			return Publication{}, fmt.Errorf("%s/%s has %d samples, want %d", result.Variant, result.Name, len(result.NsPerOp), count)
		}
		publication.Results = append(publication.Results, *result)
	}
	slices.SortFunc(publication.Results, func(a, b BenchmarkResult) int {
		if n := strings.Compare(a.Variant, b.Variant); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	slices.SortFunc(publication.Crosslang, func(a, b CrosslangResult) int {
		if n := strings.Compare(a.Implementation, b.Implementation); n != 0 {
			return n
		}
		return strings.Compare(a.Corpus, b.Corpus)
	})
	return publication, nil
}

func parseBenchmarkFile(path, initialVariant string, metadata *Metadata, results map[string]*BenchmarkResult) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	variant := initialVariant
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "benchmark-variant="):
			variant = strings.TrimPrefix(line, "benchmark-variant=")
		case strings.HasPrefix(line, "repository commit="):
			fields := strings.Fields(line)
			metadata.Commit = strings.TrimPrefix(fields[1], "commit=")
			metadata.Dirty = strings.TrimPrefix(fields[2], "dirty=") != "false"
		case strings.HasPrefix(line, "go version "):
			metadata.GoVersion = strings.TrimPrefix(line, "go version ")
		case strings.HasPrefix(line, "go-commit="):
			metadata.GoCommit = strings.TrimPrefix(line, "go-commit=")
		case strings.HasPrefix(line, "legacy-go=go version "):
			metadata.LegacyVersion = strings.TrimPrefix(line, "legacy-go=go version ")
		case strings.HasPrefix(line, "goos:") && metadata.OS == "":
			metadata.OS = strings.TrimSpace(strings.TrimPrefix(line, "goos:"))
		case strings.HasPrefix(line, "goarch:") && metadata.Arch == "":
			metadata.Arch = strings.TrimSpace(strings.TrimPrefix(line, "goarch:"))
		case strings.HasPrefix(line, "cpu:") && metadata.Machine == "":
			metadata.Machine = strings.TrimSpace(strings.TrimPrefix(line, "cpu:"))
		case strings.HasPrefix(line, "Benchmark"):
			if variant == "" {
				return fmt.Errorf("benchmark appeared before a variant marker in %s", path)
			}
			name, metrics, ok := parseBenchmarkLine(line)
			if !ok {
				continue
			}
			key := variant + "\x00" + name
			result := results[key]
			if result == nil {
				result = &BenchmarkResult{Variant: variant, Name: name}
				results[key] = result
			}
			result.NsPerOp = append(result.NsPerOp, metrics.NsPerOp)
			result.MBPerSec = append(result.MBPerSec, metrics.MBPerSec)
			result.BytesPerOp = append(result.BytesPerOp, metrics.BytesPerOp)
			result.AllocsPerOp = append(result.AllocsPerOp, metrics.AllocsPerOp)
		}
	}
	return scanner.Err()
}

func parseBenchmarkLine(line string) (string, Metrics, bool) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return "", Metrics{}, false
	}
	name := cpuSuffix.ReplaceAllString(fields[0], "")
	var metric Metrics
	for i := 1; i < len(fields); i++ {
		if i == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[i-1], 64)
		if err != nil {
			continue
		}
		switch fields[i] {
		case "ns/op":
			metric.NsPerOp = value
		case "MB/s":
			metric.MBPerSec = value
		case "B/op":
			metric.BytesPerOp = value
		case "allocs/op":
			metric.AllocsPerOp = value
		}
	}
	return name, metric, metric.NsPerOp > 0
}

func parseCrosslangFile(path string, metadata *Metadata) ([]CrosslangResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	implementation := ""
	backend := ""
	var results []CrosslangResult
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "benchmark-implementation="):
			implementation = strings.TrimPrefix(line, "benchmark-implementation=")
			backend = ""
		case strings.HasPrefix(line, "go-backend="):
			backend = strings.TrimPrefix(line, "go-backend=")
		case strings.HasPrefix(line, "crosslang-samples="):
			fields := strings.Fields(line)
			for _, field := range fields {
				switch {
				case strings.HasPrefix(field, "crosslang-samples="):
					metadata.CrossSamples, _ = strconv.Atoi(strings.TrimPrefix(field, "crosslang-samples="))
				case strings.HasPrefix(field, "crosslang-min-time="):
					metadata.CrossMinTime = strings.TrimPrefix(field, "crosslang-min-time=")
				}
			}
		case strings.Contains(line, "clang version "):
			metadata.CXXVersion = line
		case strings.HasPrefix(line, "C++ simdjson ") && strings.Contains(line, "implementation:"):
			_, metadata.CXXImpl, _ = strings.Cut(line, "implementation:")
			metadata.CXXImpl = strings.TrimSpace(metadata.CXXImpl)
		case strings.Contains(line, "contract=parse+semantic-digest"):
			if implementation == "" {
				return nil, fmt.Errorf("cross-language result appeared before implementation marker")
			}
			result, parseErr := parseCrosslangLine(line)
			if parseErr != nil {
				return nil, parseErr
			}
			result.Implementation = implementation
			result.Backend = backend
			results = append(results, result)
		}
	}
	return results, scanner.Err()
}

func parseCrosslangLine(line string) (CrosslangResult, error) {
	fields := strings.Fields(line)
	result := CrosslangResult{Corpus: strings.TrimSuffix(fields[0], ".json")}
	for i, field := range fields {
		switch {
		case strings.HasPrefix(field, "digest="):
			result.Digest = strings.TrimPrefix(field, "digest=")
		case strings.HasPrefix(field, "time="):
			value := strings.TrimPrefix(field, "time=")
			if value == "" && i+1 < len(fields) {
				value = fields[i+1]
			}
			value = strings.TrimSuffix(value, "ns")
			result.NsPerOp, _ = strconv.ParseFloat(value, 64)
		}
	}
	if result.Digest == "" || result.NsPerOp <= 0 {
		return CrosslangResult{}, fmt.Errorf("cannot parse cross-language row %q", line)
	}
	return result, nil
}
