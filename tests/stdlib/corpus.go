// Package stdlibcorpus exposes the exact high-level JSON payloads and concrete
// models from the Go revision recorded in docs/provenance.md.
package stdlibcorpus

import (
	"embed"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Names lists every payload in encoding/json/internal/jsontest at the pinned
// Go revision.
var Names = [...]string{
	"canada_geometry.json.zst",
	"citm_catalog.json.zst",
	"golang_source.json.zst",
	"string_escaped.json.zst",
	"string_unicode.json.zst",
	"synthea_fhir.json.zst",
	"twitter_status.json.zst",
}

//go:embed testdata/*.json.zst
var compressedFiles embed.FS

// Read decompresses one named corpus payload.
func Read(name string) ([]byte, error) {
	compressed, err := compressedFiles.ReadFile("testdata/" + name)
	if err != nil {
		return nil, fmt.Errorf("read stdlib corpus %q: %w", name, err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	defer decoder.Close()
	src, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress stdlib corpus %q: %w", name, err)
	}
	return src, nil
}
