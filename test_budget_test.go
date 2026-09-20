package vibejson

import "testing"

func testIterations(full, short int) int {
	if testing.Short() {
		return short
	}
	return full
}
