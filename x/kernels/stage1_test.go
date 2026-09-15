//go:build !go1.28 && goexperiment.simd && (arm64 || amd64)

package kernels

import "testing"

func TestStage1BlockAllByteValues(t *testing.T) {
	// Every byte value at every lane position, one at a time, so any lane
	// permutation inside the kernel is fully exercised.
	checkStage1BlockExhaustive(t, "Stage1Block", Stage1Block, stage1BlockBytewise)
}

func TestStage1BlockBracketsAllByteValues(t *testing.T) {
	// Every byte value at every lane position, one at a time, so any lane
	// permutation inside the kernel is fully exercised. The bracket fold in
	// particular must not admit any byte outside the six-character class.
	checkStage1BlockExhaustive(t, "Stage1BlockBrackets", Stage1BlockBrackets, stage1BracketsBytewise)
}

func TestStage1BlockBracketsRandom(t *testing.T) {
	interesting := []byte{'"', '\\', '{', '}', '[', ']', ':', ',', ' ', 0x02, 0x1f, 0x5b, 0x5d, 0x7c, 0x80, 0xfb, 0xfd, 'a', '0'}
	checkStage1BlockRandom(t, "Stage1BlockBrackets", 20000, 0x9e3779b97f4a7c15, interesting, Stage1BlockBrackets, stage1BracketsBytewise)
}

var stage1BenchSink Stage1Masks

func BenchmarkStage1Block(b *testing.B) {
	var block [64]byte
	copy(block[:], `{"key": "value", "n": 12345, "flag": true, "arr": [1,2,3]}   `)
	b.SetBytes(64)
	for i := 0; i < b.N; i++ {
		Stage1Block(&block, &stage1BenchSink)
	}
}

func TestStage1BlockRandom(t *testing.T) {
	interesting := []byte{'"', '\\', '{', '}', '[', ']', ':', ',', ' ', '\t', '\n', '\r', 0x00, 0x1f, 0x7f, 0x80, 0xff, 'a', '0'}
	checkStage1BlockRandom(t, "Stage1Block", 20000, 0x9e3779b97f4a7c15, interesting, Stage1Block, stage1BlockBytewise)
}
