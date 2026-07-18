package simdjson

import "testing"

// testIterations keeps exhaustive and high-volume differentials unchanged in
// normal runs while giving race/checkptr and other -short instrumentation runs
// a representative sample of every generator and code path.
func testIterations(full, short int) int {
	if testing.Short() {
		return short
	}
	return full
}
