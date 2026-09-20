package simd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/thesyncim/vibejson/x/floatconv"
)

func TestFormatPowerOfTenMatchesLegacyScaleTable(t *testing.T) {
	hash := sha256.New()
	var words [16]byte
	for exp10 := -348; exp10 <= 347; exp10++ {
		entry := floatPow10[exp10-floatPow10Min]
		hi, lo := entry.hi, entry.lo
		wantHi, wantLo := floatconv.FormatPowerOfTen(exp10)
		if hi != wantHi || lo != wantLo {
			t.Fatalf("formatter power %d = (%#x, %#x), want (%#x, %#x)", exp10, hi, lo, wantHi, wantLo)
		}
		binary.BigEndian.PutUint64(words[:8], hi)
		binary.BigEndian.PutUint64(words[8:], lo)
		_, _ = hash.Write(words[:])
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != "6398a6ef4eb35c75400c3c68282afbff539ef8ee5e95b03f5616ca431dc8c84a" {
		t.Fatalf("formatter power-table digest = %s", got)
	}
}
