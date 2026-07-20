//go:build arm64

package kernels

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The five arm64 kernels each carry two hand-unrolled copies of the
// bit-indexer scatter region (Provenance: CPP-STAGE1-001): a block-loop copy
// driven by mask/emitBase and a chunk-tail copy driven by
// pendingMask/pendingBase. The copies are deliberately inline — a call in
// these loops is the one cost the region exists to avoid, and generated
// source was judged harder to maintain than visible duplication — so this
// test is the drift guard: it extracts every copy, normalizes the two
// driving names and comments, and fails if any copy differs from the rest.
// If it fires, apply the same edit to every listed copy.
func TestScatterRegionCopiesIdentical(t *testing.T) {
	files := []string{
		"stage1_index_arm64.go",
		"structural_cursor_arm64.go",
		"structural_index_meta_arm64.go",
		"structural_valid_arm64.go",
		"structural_valid_coarse_arm64.go",
	}

	start := regexp.MustCompile(`^\s*n := bits\.OnesCount64\((mask|pendingMask)\)$`)
	end := regexp.MustCompile(`^\s*written \+= n$`)
	comment := regexp.MustCompile(`^\s*//`)
	names := strings.NewReplacer(
		"pendingMask", "mask",
		"pendingBase", "emitBase",
	)

	var reference []string
	var origin string
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		copies := 0
		for i := 0; i < len(lines); i++ {
			if !start.MatchString(lines[i]) {
				continue
			}
			// The loop and tail copies declare output and rev in either
			// order; require each exactly once but compare them
			// order-insensitively.
			var region, declarations []string
			for j := i; j < len(lines); j++ {
				if comment.MatchString(lines[j]) {
					continue
				}
				line := names.Replace(strings.TrimSpace(lines[j]))
				if strings.HasPrefix(line, "output := ") || strings.HasPrefix(line, "rev := ") {
					declarations = append(declarations, line)
					continue
				}
				region = append(region, line)
				if end.MatchString(lines[j]) {
					i = j
					break
				}
			}
			sort.Strings(declarations)
			region = append(region, declarations...)
			copies++
			if reference == nil {
				reference = region
				origin = name
				continue
			}
			if len(region) != len(reference) {
				t.Fatalf("%s scatter copy %d has %d lines, %s has %d — update every copy in lockstep",
					name, copies, len(region), origin, len(reference))
			}
			for k := range region {
				if region[k] != reference[k] {
					t.Fatalf("%s scatter copy %d drifted at %q (reference %s has %q) — update every copy in lockstep",
						name, copies, region[k], origin, reference[k])
				}
			}
		}
		if copies != 2 {
			t.Fatalf("%s: found %d scatter copies, want 2 (loop and tail); if the region moved, update this test's anchors", name, copies)
		}
	}
}
