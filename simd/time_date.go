// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in LICENSE-GO.

// Provenance: GO-DATE-001. Adapted from Go commit
// 03845e30f7b73d1703bd8c21017297f6eecb76d6, src/time/time.go absDays.date
// and helpers. The underlying algorithm is Neri and Schneider (2023), DOI
// 10.1002/spe.3172. Local changes integrate direct JSON time formatting; see
// docs/provenance.md.

package simd

import "math/bits"

// This file ports the standard library time package's absolute-time civil
// date computation (the Neri-Schneider algorithm and the epoch constants it
// depends on), so AppendTime can derive every calendar field from a single
// absolute-seconds value instead of separate Date, Clock, and Zone calls
// that each repeat the location lookup and epoch shift.

const (
	secondsPerDay = 86400

	// absoluteYears shifts the epoch to March 1 of a year divisible by 400
	// and far enough back that every representable time is nonnegative.
	absoluteYears     = 292277022400
	marchThruDecember = 306

	absoluteToInternal int64 = -(absoluteYears*365.2425 + marchThruDecember) * secondsPerDay
	unixToInternal     int64 = (1969*365 + 1969/4 - 1969/100 + 1969/400) * secondsPerDay

	// unixToAbsolute converts a Unix time in seconds to absolute seconds.
	unixToAbsolute = unixToInternal - absoluteToInternal
)

// absDaysToDate converts days since the absolute epoch into a standard
// year, 1-based month, and 1-based day. The wrapped constants and shifted
// multiplications mirror time.absDays.date exactly, so out-of-range inputs
// wrap to the same years the standard library reports.
func absDaysToDate(days uint64) (year int, month, day uint32) {
	d := 4*days + 3
	century := d / 146097
	cd := uint32(d%146097) | 3

	// cyear = cd/1461, ayday = cd%1461/4, as one 32x32 multiply.
	cyear, lo := bits.Mul32(2939745, cd)
	ayday := lo / 2939745 / 4

	// amonth = (5*ayday+461)/153 with March as month 3, wrapping into the
	// next year for January and February; mday is its 1-based remainder.
	md := 2141*ayday + 197913
	day = (md&0xFFFF)/2141 + 1
	janFeb := uint32(0)
	if ayday >= marchThruDecember {
		janFeb = 1
	}
	year = int(uint64(century)*100-absoluteYears) + int(cyear) + int(janFeb)
	month = md>>16 - janFeb*12
	return year, month, day
}
