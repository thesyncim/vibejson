package slopjson

import "sync/atomic"

const (
	marshalSizeHintMin    uint64 = 64
	marshalSizeHintMax    uint64 = 256 << 10
	marshalSizeHintGrowth uint64 = 8

	marshalSizeHintUnconfirmed uint64 = 1 << 63
	marshalSizeHintSizeMask           = marshalSizeHintUnconfirmed - 1
)

// loadMarshalSizeHint returns the capacity for the next convenience Marshal
// call. One oversized observation is kept as an integer candidate but receives
// only a small allocation budget; a repeated equal observation confirms a
// stable large workload and restores exact presizing.
func loadMarshalSizeHint(hint *atomic.Uint64) int {
	state := hint.Load()
	size := state & marshalSizeHintSizeMask
	if state&marshalSizeHintUnconfirmed != 0 {
		size = marshalSizeHintMin * marshalSizeHintGrowth
	}
	if size < marshalSizeHintMin {
		return int(marshalSizeHintMin)
	}
	return int(size)
}

// updateMarshalSizeHint records observed without letting one exceptional value
// poison every later allocation. The state retains no output memory: the high
// bit distinguishes an unconfirmed large integer candidate from a confirmed
// exact size. Smaller results replace either state immediately.
func updateMarshalSizeHint(hint *atomic.Uint64, observed uint64) {
	if observed > marshalSizeHintSizeMask {
		observed = marshalSizeHintSizeMask
	}
	stored := hint.Load()
	for {
		if stored&marshalSizeHintUnconfirmed == 0 && stored == observed {
			return
		}
		var next uint64
		switch {
		case observed <= marshalSizeHintMax:
			next = observed
		case stored&marshalSizeHintUnconfirmed != 0 && stored&marshalSizeHintSizeMask == observed:
			next = observed
		default:
			next = marshalSizeHintUnconfirmed | observed
		}
		if stored == next || hint.CompareAndSwap(stored, next) {
			return
		}
		stored = hint.Load()
	}
}
