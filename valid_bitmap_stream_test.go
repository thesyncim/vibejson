package simdjson

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streamedOracle compares the batched engine against the per-block engine
// and the scalar Validate on one input. All engines must agree on decided
// and, when decided, on the verdict. This is the consumer-level differential
// that guards the per-block and batched classification paths against
// divergence.
func streamedOracle(t *testing.T, src []byte, label string) {
	t.Helper()
	streamedOracleVerdict(t, src, label, Validate(src) == nil)
}

func streamedOracleVerdict(t *testing.T, src []byte, label string, want bool) {
	t.Helper()
	refOK, refDecided := validBitmapPerBlock(src)
	gpOK, gpDecided := validBitmapStreamed(src)
	if gpDecided != refDecided {
		t.Fatalf("%s: decided mismatch: perBlock=%v streamed=%v (len %d)\n%.200q",
			label, refDecided, gpDecided, len(src), src)
	}
	if !refDecided {
		return
	}
	if refOK != want || gpOK != want {
		t.Fatalf("%s: verdict mismatch: perBlock=%v streamed=%v, Validate=%v (len %d)\n%.200q",
			label, refOK, gpOK, want, len(src), src)
	}
}

func TestValidBitmapStreamedMatchesScalarOnTestSuite(t *testing.T) {
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	indent := "\n" + strings.Repeat(" ", 10)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		streamedOracle(t, data, entry.Name())

		var wrapped bytes.Buffer
		wrapped.WriteString("[")
		for range 8 {
			wrapped.WriteString(indent)
			wrapped.Write(data)
			wrapped.WriteString(",")
		}
		wrapped.WriteString(indent)
		wrapped.Write(data)
		wrapped.WriteString("\n]")
		streamedOracle(t, wrapped.Bytes(), "wrapped "+entry.Name())
	}
}

// buildWhitespaceHeavyDoc constructs the deterministic pretty-printed
// benchmark document: nested records with strings, escapes, numbers,
// and literals under the given indent.
func buildWhitespaceHeavyDoc(tb testing.TB, indent string) []byte {
	type entry struct {
		Name    string    `json:"name"`
		Text    string    `json:"text"`
		Value   float64   `json:"value"`
		Count   int64     `json:"count"`
		Flag    bool      `json:"flag"`
		Nothing *int      `json:"nothing"`
		Scores  []float64 `json:"scores"`
		Tags    []string  `json:"tags"`
	}
	rng := rand.New(rand.NewPCG(21, 42))
	texts := []string{
		"plain ascii words in a sentence", "tab\tand\nnewline", `quote " backslash \ slash /`,
		"payload with more text than structure", " leading and trailing ", "<html> & entities >",
	}
	var entries []entry
	for len(entries) < 500 {
		entries = append(entries, entry{
			Name:   texts[rng.IntN(len(texts))],
			Text:   texts[rng.IntN(len(texts))],
			Value:  rng.Float64() * 1e6,
			Count:  rng.Int64(),
			Flag:   rng.IntN(2) == 0,
			Scores: []float64{rng.Float64(), -rng.Float64() * 1e-7, 0, 1e21},
			Tags:   []string{"alpha", "beta", "gamma"},
		})
	}
	doc, err := json.MarshalIndent(entries, "", indent)
	if err != nil {
		tb.Fatal(err)
	}
	if len(doc) < validBitmapMinBytes {
		tb.Fatalf("benchmark document too small: %d", len(doc))
	}
	return doc
}

// benchmarkBitmapEngines runs both engines interleaved-by-count on one
// document, after asserting that every engine commits and agrees.
func benchmarkBitmapEngines(b *testing.B, doc []byte) {
	if ok, decided := validBitmapPerBlock(doc); !decided || !ok {
		b.Fatalf("per-block engine: ok=%v decided=%v (len %d)", ok, decided, len(doc))
	}
	if ok, decided := validBitmapStreamed(doc); !decided || !ok {
		b.Fatal("streamed engine did not commit")
	}
	b.Run("perBlock", func(b *testing.B) {
		b.SetBytes(int64(len(doc)))
		for i := 0; i < b.N; i++ {
			if ok, _ := validBitmapPerBlock(doc); !ok {
				b.Fatal("invalid")
			}
		}
	})
	b.Run("batchedGP", func(b *testing.B) {
		b.SetBytes(int64(len(doc)))
		for i := 0; i < b.N; i++ {
			if ok, _ := validBitmapStreamed(doc); !ok {
				b.Fatal("invalid")
			}
		}
	})
}

// buildNestedTwoSpaceDoc builds the borderline document: 2-space indent
// with deeper nesting, denser emits per block, just above the engine's
// commitment ratio.
func buildNestedTwoSpaceDoc(tb testing.TB) []byte {
	type group struct {
		Label   string           `json:"label"`
		Entries []map[string]any `json:"entries"`
	}
	var groups []group
	for len(groups) < 400 {
		groups = append(groups, group{
			Label: "group with a plain label",
			Entries: []map[string]any{
				{"name": "first item name", "value": 12345.25, "flag": true},
				{"name": "second item with text", "note": "quote \" and backslash \\", "count": 7},
			},
		})
	}
	doc, err := json.MarshalIndent(map[string]any{"groups": groups}, "", "  ")
	if err != nil {
		tb.Fatal(err)
	}
	if len(doc) < validBitmapMinBytes {
		tb.Fatalf("benchmark document too small: %d", len(doc))
	}
	return doc
}

// buildProsePayloadDoc builds a compact string-heavy document: records of
// long prose values behind short keys, no indentation. Roughly three
// quarters of the sampled bytes are inside strings and emits stay sparse,
// so the sampler's string leg commits it — the twitter_status shape.
func buildProsePayloadDoc(tb testing.TB) []byte {
	var out strings.Builder
	out.WriteString(`[`)
	for i := 0; out.Len() < validBitmapMinBytes+4096; i++ {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(`{"text":"a status update written in plain prose, long enough that the payload dwarfs the punctuation around it","lang":"en","note":"more prose follows the first sentence and keeps the string share high"}`)
	}
	out.WriteString(`]`)
	return []byte(out.String())
}

// buildEscapeDenseDoc builds a string-heavy document whose strings are
// full of escapes — about one escape per six string bytes, the
// string_escaped shape. The sampler's escape guard must refuse it.
func buildEscapeDenseDoc(tb testing.TB) []byte {
	var out strings.Builder
	out.WriteString(`[`)
	for i := 0; out.Len() < validBitmapMinBytes+4096; i++ {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(`{"text":"line\none\ttab\"quote\\slash\nline\ntwo\ttab\"q\\s\nmore\nrows\there"}`)
	}
	out.WriteString(`]`)
	return []byte(out.String())
}

// TestValidBitmapRouting pins the sampler's routing decision per document
// shape, for every engine: the two samplers are separate code (Go per-block
// and Go streamed) applying one rule
// (validBitmapSampleCommit), so identical routing is a contract, not a
// coincidence. The expectations encode the rule's intent: whitespace-heavy
// and prose-heavy shapes commit, compact records, escape-dense strings,
// and number-dense shapes refuse.
func TestValidBitmapRouting(t *testing.T) {
	cases := []struct {
		label       string
		src         []byte
		wantDecided bool
	}{
		{"indent4", buildWhitespaceHeavyDoc(t, "    "), true},
		{"nested2", buildNestedTwoSpaceDoc(t), true},
		{"prose payload", buildProsePayloadDoc(t), true},
		{"escape dense", buildEscapeDenseDoc(t), false},
		{"compact records", benchRecordsJSON(1024), false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			refOK, refDecided := validBitmapPerBlock(tc.src)
			gpOK, gpDecided := validBitmapStreamed(tc.src)
			if refDecided != tc.wantDecided || gpDecided != tc.wantDecided {
				t.Fatalf("decided: perBlock=%v streamed=%v, want %v (len %d)",
					refDecided, gpDecided, tc.wantDecided, len(tc.src))
			}
			if tc.wantDecided && (!refOK || !gpOK) {
				t.Fatal("engine rejected a valid document")
			}
		})
	}
}

// BenchmarkValidBitmapIndent4: deep 4-space indentation, the engine's
// home turf (whitespace dominates, sparse emits).
func BenchmarkValidBitmapIndent4(b *testing.B) {
	benchmarkBitmapEngines(b, buildWhitespaceHeavyDoc(b, "    "))
}

// BenchmarkValidBitmapProse: compact string-heavy prose records, the
// string leg's home turf (string interiors dominate, sparse emits).
func BenchmarkValidBitmapProse(b *testing.B) {
	benchmarkBitmapEngines(b, buildProsePayloadDoc(b))
}

// BenchmarkValidBitmapNested2: 2-space indent, nested containers,
// denser emits, near the engine's commitment boundary.
func BenchmarkValidBitmapNested2(b *testing.B) {
	benchmarkBitmapEngines(b, buildNestedTwoSpaceDoc(b))
}
