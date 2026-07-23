//go:build !race

package slopjson

// raceEnabled reports whether the test binary was built with -race. See
// race_on_test.go for why allocation assertions consult it.
const raceEnabled = false
