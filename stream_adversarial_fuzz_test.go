package simdjson

import (
	"bytes"
)

// adversarialStreamCorpus builds streaming inputs engineered to stress the
// resumable framer at chunk boundaries: dense escape runs, escaped quotes and
// backslashes, huge string bodies, deeply nested containers, and brackets that
// live inside strings so only correct in-string tracking frames them.
func adversarialStreamCorpus() [][]byte {
	bigStr := append(append([]byte{'"'}, bytes.Repeat([]byte("x"), 5000)...), '"')
	escRun := append(append([]byte{'"'}, bytes.Repeat([]byte(`\\`), 1000)...), '"')
	escQuotes := append(append([]byte{'"'}, bytes.Repeat([]byte(`a\"b`), 500)...), '"')
	deep := append(bytes.Repeat([]byte("["), 400), bytes.Repeat([]byte("]"), 400)...)
	deepObj := append(bytes.Repeat([]byte(`{"k":`), 200), append([]byte("0"), bytes.Repeat([]byte("}"), 200)...)...)
	bracketsInStr := []byte(`{"a":"}]}]}]","b":[{"c":"[[[["}]}`)
	unicodeEsc := append(append([]byte{'"'}, bytes.Repeat([]byte(`𝄞`), 100)...), '"')
	trailingBackslashes := append(append([]byte{'"'}, bytes.Repeat([]byte(`\\`), 999)...), '\\', '"', '"')
	corpus := [][]byte{
		bigStr,
		escRun,
		escQuotes,
		deep,
		deepObj,
		bracketsInStr,
		unicodeEsc,
		trailingBackslashes,
		[]byte(`"\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"`),
		[]byte("[1,2,3]\n{\"a\":\"b\\\"c\"}\ntrue\nnull\n1.5e300\n"),
		bytes.Repeat([]byte(`{"k":"v"}`+"\n"), 50),
	}
	// Concatenations of several adversarial values back to back, no separators.
	var joined []byte
	for _, c := range corpus {
		joined = append(joined, c...)
	}
	corpus = append(corpus, joined)
	return corpus
}
