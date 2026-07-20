package simdjson

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func bitmapOracleVerdict(t *testing.T, src []byte, label string, want bool) {
	t.Helper()
	got, decided := validBitmap(src)
	if !decided {
		return
	}
	if got != want {
		t.Fatalf("%s: validBitmap = %v, scalar validator = %v (len %d)\n%.200q", label, got, want, len(src), src)
	}
}

func bitmapOracleStrict(t *testing.T, src []byte, label string, want bool) {
	t.Helper()
	got, decided := validBitmap(src)
	if !decided {
		t.Fatalf("%s: production-sized bitmap input declined (len %d)", label, len(src))
	}
	if got != want {
		t.Fatalf("%s: validBitmap = %v, scalar validator = %v (len %d)\n%.200q",
			label, got, want, len(src), src)
	}
}

func bitmapRoutedInput(src []byte) []byte {
	doc := make([]byte, validBitmapMinBytes+len(src))
	for i := 0; i < validBitmapMinBytes; i++ {
		doc[i] = ' '
	}
	copy(doc[validBitmapMinBytes:], src)
	return doc
}

func TestValidBitmapMatchesScalarOnTestSuite(t *testing.T) {
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		routed := bitmapRoutedInput(data)
		bitmapOracleStrict(t, routed, entry.Name(), ValidateOptions(data, Options{}) == nil)
	}
}

func TestValidBitmapSampleBoundary(t *testing.T) {
	for _, size := range []int{0, 1, 63, 64, validBitmapSampleBlocks*64 - 1} {
		if ok, decided := validBitmap(bytes.Repeat([]byte{' '}, size)); ok || decided {
			t.Fatalf("size %d: validBitmap = %v, %v; want decline", size, ok, decided)
		}
	}
	valid := bytes.Repeat([]byte{' '}, validBitmapSampleBlocks*64)
	valid[len(valid)-1] = '0'
	bitmapOracleStrict(t, valid, "exact sample valid", true)
	invalid := append([]byte(nil), valid...)
	invalid[len(invalid)-1] = 'x'
	bitmapOracleStrict(t, invalid, "exact sample invalid", false)
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

// TestValidBitmapRouting pins the production sampler's decision per document
// shape. Whitespace-heavy and prose-heavy shapes commit; compact records,
// escape-dense strings, and number-dense shapes refuse.
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
		{"below sample", []byte("null"), false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ok, decided := validBitmap(tc.src)
			if decided != tc.wantDecided {
				t.Fatalf("decided=%v, want %v (len %d)", decided, tc.wantDecided, len(tc.src))
			}
			if tc.wantDecided && !ok {
				t.Fatal("engine rejected a valid document")
			}
		})
	}
}

// TestBitmapMutationDifferentials shares one deterministic mutation corpus
// across the packed validator and index builder. Each mutant and scalar
// verdict is prepared only once.
func TestBitmapMutationDifferentials(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	var indexBufs indexOracleBufs

	bitmapOracleVerdict(t, doc, "base document", true)
	if ok, decided := validBitmap(doc); !decided || !ok {
		t.Fatalf("base document: ok=%v decided=%v", ok, decided)
	}
	indexBitmapOracle(t, doc, &indexBufs, true, "base document")

	rng := rand.New(rand.NewPCG(7, 13))
	iterations := testIterations(20_000, 128)
	for mutant := 0; mutant < iterations; mutant++ {
		mutated := append([]byte(nil), doc...)
		switch rng.IntN(4) {
		case 0: // byte substitution
			pos := rng.IntN(len(mutated))
			mutated[pos] = byte(rng.IntN(256))
		case 1: // substitution from a hostile alphabet
			pos := rng.IntN(len(mutated))
			hostile := []byte(`"\{}[]:,0x eEtfn.+-` + "\x00\x1f\x80\xe2\xff")
			mutated[pos] = hostile[rng.IntN(len(hostile))]
		case 2: // deletion
			pos := rng.IntN(len(mutated))
			mutated = append(mutated[:pos], mutated[pos+1:]...)
		case 3: // truncation
			mutated = mutated[:rng.IntN(len(mutated))]
		}
		want := ValidateOptions(mutated, Options{}) == nil
		bitmapOracleVerdict(t, mutated, "mutant", want)
		indexBitmapOracle(t, mutated, &indexBufs, false, "mutant")
	}
}

var bitmapTestDocumentCache struct {
	sync.Once
	doc []byte
	err error
}

func buildBitmapTestDocument(t *testing.T) []byte {
	t.Helper()
	type leaf struct {
		Name    string    `json:"name"`
		Text    string    `json:"text"`
		Value   float64   `json:"value"`
		Count   int64     `json:"count"`
		Flag    bool      `json:"flag"`
		Nothing *int      `json:"nothing"`
		Scores  []float64 `json:"scores"`
	}
	type node struct {
		Leaf     leaf              `json:"leaf"`
		Children []leaf            `json:"children"`
		Index    map[string]string `json:"index"`
	}
	bitmapTestDocumentCache.Do(func() {
		rng := rand.New(rand.NewPCG(3, 5))
		texts := []string{
			"plain ascii", "tab\tand\nnewline", `quote " backslash \ slash /`,
			"unicode   line sep é日本語", `escape A𝄞 mix`,
			"", " leading and trailing ", "<html> & entities >",
		}
		var nodes []node
		for i := 0; len(nodes) < 64; i++ {
			var children []leaf
			for range rng.IntN(5) {
				children = append(children, leaf{
					Name:   texts[rng.IntN(len(texts))],
					Text:   texts[rng.IntN(len(texts))],
					Value:  rng.Float64() * 1e6,
					Count:  rng.Int64(),
					Flag:   rng.IntN(2) == 0,
					Scores: []float64{rng.Float64(), -rng.Float64() * 1e-7, 0, 1e21},
				})
			}
			nodes = append(nodes, node{
				Leaf:     leaf{Name: texts[rng.IntN(len(texts))], Scores: []float64{}},
				Children: children,
				Index:    map[string]string{"a b": texts[rng.IntN(len(texts))], "c\td": "e"},
			})
		}
		bitmapTestDocumentCache.doc, bitmapTestDocumentCache.err = json.MarshalIndent(nodes, "", "    ")
	})
	if bitmapTestDocumentCache.err != nil {
		t.Fatal(bitmapTestDocumentCache.err)
	}
	if len(bitmapTestDocumentCache.doc) < validBitmapMinBytes {
		t.Fatalf("test document too small: %d", len(bitmapTestDocumentCache.doc))
	}
	return bitmapTestDocumentCache.doc
}
