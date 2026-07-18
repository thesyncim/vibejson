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

// bitmapOracle compares validBitmap against the scalar Validate on one
// input whenever the density sampler takes the case.
func bitmapOracle(t *testing.T, src []byte, label string) {
	t.Helper()
	bitmapOracleVerdict(t, src, label, Validate(src) == nil)
}

func bitmapOracleVerdict(t *testing.T, src []byte, label string, want bool) {
	t.Helper()
	got, decided := validBitmap(src)
	if !decided {
		return
	}
	if got != want {
		t.Fatalf("%s: validBitmap = %v, Validate = %v (len %d)\n%.200q", label, got, want, len(src), src)
	}
}

func TestValidBitmapMatchesScalarOnTestSuite(t *testing.T) {
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	// Indentation wrapper pushes every case through string, escape, and
	// scalar edge handling at the whitespace levels the engine dispatches on.
	indent := "\n" + strings.Repeat(" ", 10)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		bitmapOracle(t, data, entry.Name())

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
		bitmapOracle(t, wrapped.Bytes(), "wrapped "+entry.Name())
	}
}

// TestBitmapMutationDifferentials shares one deterministic mutation corpus
// across the bitmap, streamed, index, and stage-2 engines. The former separate
// batteries copied and rescanned four large documents per logical mutation.
// Every engine retains its original 20k cases (8k for the streamed engine),
// while each mutant and scalar verdict is now prepared only once.
func TestBitmapMutationDifferentials(t *testing.T) {
	doc := buildBitmapTestDocument(t)
	var indexBufs indexOracleBufs

	bitmapOracleVerdict(t, doc, "base document", true)
	if ok, decided := validBitmap(doc); !decided || !ok {
		t.Fatalf("base document: ok=%v decided=%v", ok, decided)
	}
	streamedOracleVerdict(t, doc, "base document", true)
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
		want := Validate(mutated) == nil
		bitmapOracleVerdict(t, mutated, "mutant", want)
		if mutant < min(iterations, 8_000) {
			streamedOracleVerdict(t, mutated, "mutant", want)
		}
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

// FuzzValidBitmap compares the bitmap engine with the scalar validator on
// arbitrary inputs whenever the engine takes the case.
func FuzzValidBitmap(f *testing.F) {
	f.Add([]byte(`{"a": [1, 2.5e-3, true, false, null, "x\nA"]}`))
	f.Add([]byte("[\n  \"" + strings.Repeat("word ", 40) + "\\u2028\",\n  -0.125e+9\n]"))
	f.Add(bytes.Repeat([]byte(`{"k": "v", "n": [1,2,3]} `), 40))
	f.Fuzz(func(t *testing.T, src []byte) {
		got, decided := validBitmap(src)
		if !decided {
			return
		}
		want := Validate(src) == nil
		if got != want {
			t.Fatalf("validBitmap = %v, Validate = %v on %q", got, want, src)
		}
	})
}
