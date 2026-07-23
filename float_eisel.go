package slopjson

import "github.com/thesyncim/slopjson/internal/floatconv"

// Keep the root adapter so all decoder variants, including generated sources,
// share one stable call site while the conversion kernel and its generated
// table remain owned by their internal package.
func eiselLemire64(man uint64, exp10 int, neg bool) (float64, bool) {
	return floatconv.EiselLemire64(man, exp10, neg)
}
