//go:build race

package vibejson

// raceEnabled reports whether the test binary was built with -race. The race
// detector instruments allocation and disables sync.Pool reuse, so exact
// allocation-count assertions are skipped under it.
const raceEnabled = true
