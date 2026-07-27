//go:build go1.27 && !go1.28 && goexperiment.simd && amd64

package scanner

import "testing"

func TestAMD64ScannerSelectionRequiresProvenWidth(t *testing.T) {
	cases := []struct {
		name    string
		hasAVX2 bool
		want    uint8
	}{
		{"scalar", false, scanLevelScalar},
		{"avx2", true, scanLevelAVX2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectAMD64ScannerLevel(tc.hasAVX2); got != tc.want {
				t.Fatalf("selectAMD64ScannerLevel(%v) = %d, want %d", tc.hasAVX2, got, tc.want)
			}
		})
	}
}

func TestAMD64ScannerCrossoverMatchesScalar(t *testing.T) {
	originalLevel := scanAMD64Level
	defer func() { scanAMD64Level = originalLevel }()
	levels := []uint8{scanLevelScalar}
	if originalLevel == scanLevelAVX2 {
		levels = append(levels, scanLevelAVX2)
	}
	lengths := []int{32, 33, 38, 39, 40, 47, 48, 55, 56, 64}
	positions := []int{0, 5, 15, 16, 23, 24, 31, 32, 39, 40, 47, 48, 55, 63}
	for _, level := range levels {
		scanAMD64Level = level
		for _, length := range lengths {
			clean := longScanCase(length, -1, 0)
			for start := 0; start <= length; start++ {
				if got, want := scanStringSpecial(clean, start), scanStringSpecialScalar(clean, start); got != want {
					t.Fatalf("level=%d clean length=%d start=%d: got %d, want %d", level, length, start, got, want)
				}
			}
			for _, position := range append(positions, length-1) {
				if position >= length {
					continue
				}
				for _, special := range []byte{'"', '\\', 0x1f, 0x80} {
					src := longScanCase(length, position, special)
					for start := 0; start <= length; start++ {
						if got, want := scanStringSpecial(src, start), scanStringSpecialScalar(src, start); got != want {
							t.Fatalf("level=%d length=%d position=%d special=%#02x start=%d: got %d, want %d", level, length, position, special, start, got, want)
						}
					}
				}
			}
		}
	}
}
