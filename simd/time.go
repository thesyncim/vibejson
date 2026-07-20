package simd

import (
	"errors"
	"slices"
	"time"
)

var (
	errTimeYear = errors.New("Time.AppendText: year outside of range [0,9999]")
	errTimeZone = errors.New("Time.AppendText: timezone hour outside of range [0,23]")
)

const (
	timePrefixBytes = len(`"2006-01-02T15:04:05`)
	maxTimeBytes    = len(time.RFC3339Nano) + 2

	timeCachePrefix = 1 << 0
	timeCacheDate   = 1 << 1
)

// TimeCache memoizes the fixed-width date-time prefix across AppendTimeCached
// calls. Timestamps in real documents cluster heavily by second and by day,
// so consecutive appends usually reuse the cached twenty-byte prefix or its
// date half instead of redoing the calendar computation and digit stores.
// The zero value is an empty cache; a cache must not be shared concurrently.
type TimeCache struct {
	absSec uint64
	days   uint64
	state  uint8
	prefix [timePrefixBytes]byte
}

// AppendTime appends value as a quoted JSON string containing the RFC 3339 form
// used by time.Time.AppendText. Its fixed-width date and clock digits use the
// effective digit-formatting implementation, which may be scalar. The returned
// slice is caller-owned and may reuse or grow dst. An error returns dst
// unchanged.
func AppendTime(dst []byte, value time.Time) ([]byte, error) {
	return AppendTimeCached(dst, value, nil)
}

// AppendTimeCached is AppendTime with a caller-owned prefix memo. A nil cache
// is allowed and simply disables memoization.
func AppendTimeCached(dst []byte, value time.Time, cache *TimeCache) ([]byte, error) {
	// One zone lookup feeds every calendar field. Calling Date, Clock, and
	// Zone instead repeats the location lookup and epoch shift three times.
	_, offset := value.Zone()
	abs := uint64(value.Unix() + int64(offset) + unixToAbsolute)

	if cache != nil && cache.state&timeCachePrefix != 0 && cache.absSec == abs {
		// An equal absolute second reproduces the whole cached prefix, and
		// its cached year is known in range. The zone is still per-call.
		zoneMinutes := offset / 60
		if zoneMinutes <= -24*60 || zoneMinutes >= 24*60 {
			return dst, errTimeZone
		}
		start := len(dst)
		if cap(dst)-start < maxTimeBytes {
			dst = growTime(dst)
		}
		dst = dst[:start+maxTimeBytes]
		out := dst[start:]
		*(*[timePrefixBytes]byte)(out) = cache.prefix
		return dst[:start+appendTimeTail(out, value.Nanosecond(), offset, zoneMinutes)], nil
	}

	days := abs / secondsPerDay
	clock := uint32(abs % secondsPerDay)
	hour := clock / 3600
	minute := clock / 60 % 60
	second := clock % 60

	if cache != nil && cache.state&timeCacheDate != 0 && cache.days == days {
		// Same calendar day: reuse the cached date half and store only the
		// six clock digits; the separators are already in the prefix.
		zoneMinutes := offset / 60
		if zoneMinutes <= -24*60 || zoneMinutes >= 24*60 {
			return dst, errTimeZone
		}
		start := len(dst)
		if cap(dst)-start < maxTimeBytes {
			dst = growTime(dst)
		}
		dst = dst[:start+maxTimeBytes]
		out := dst[start:]
		*(*[timePrefixBytes]byte)(out) = cache.prefix
		out[12] = byte('0' + hour/10)
		out[13] = byte('0' + hour%10)
		out[15] = byte('0' + minute/10)
		out[16] = byte('0' + minute%10)
		out[18] = byte('0' + second/10)
		out[19] = byte('0' + second%10)
		cache.absSec = abs
		cache.prefix = *(*[timePrefixBytes]byte)(out)
		cache.state = timeCachePrefix | timeCacheDate
		return dst[:start+appendTimeTail(out, value.Nanosecond(), offset, zoneMinutes)], nil
	}

	year, month, day := absDaysToDate(days)
	if uint(year) >= 10_000 {
		return dst, errTimeYear
	}
	zoneMinutes := offset / 60
	if zoneMinutes <= -24*60 || zoneMinutes >= 24*60 {
		return dst, errTimeZone
	}

	start := len(dst)
	if cap(dst)-start < maxTimeBytes {
		dst = growTime(dst)
	}
	dst = dst[:start+maxTimeBytes]
	out := dst[start:]

	storeDateTimeParts((*[20]byte)(out), uint32(year), month, day, hour, minute, second)
	if cache != nil {
		cache.absSec = abs
		cache.days = days
		cache.prefix = *(*[timePrefixBytes]byte)(out)
		cache.state = timeCachePrefix | timeCacheDate
	}
	return dst[:start+appendTimeTail(out, value.Nanosecond(), offset, zoneMinutes)], nil
}

// appendTimeTail writes the fraction, zone, and closing quote after the fixed
// twenty-byte prefix and returns the total encoded length. out must hold
// maxTimeBytes and the zone must be validated already.
func appendTimeTail(out []byte, nanosecond, offset, zoneMinutes int) int {
	i := timePrefixBytes
	if nanosecond != 0 {
		fractionDigits := 9
		for nanosecond%10 == 0 {
			nanosecond /= 10
			fractionDigits--
		}
		out[i] = '.'
		i++
		var fraction [8]byte
		if fractionDigits == 9 {
			out[i] = byte(nanosecond/100_000_000) + '0'
			Store8Digits(&fraction, uint64(nanosecond%100_000_000))
			copy(out[i+1:], fraction[:])
		} else {
			Store8Digits(&fraction, uint64(nanosecond))
			copy(out[i:], fraction[8-fractionDigits:])
		}
		i += fractionDigits
	}
	if offset == 0 {
		out[i] = 'Z'
		i++
	} else {
		if zoneMinutes < 0 {
			out[i] = '-'
			zoneMinutes = -zoneMinutes
		} else {
			out[i] = '+'
		}
		out[i+1] = byte(zoneMinutes/600) + '0'
		out[i+2] = byte(zoneMinutes/60%10) + '0'
		out[i+3] = ':'
		out[i+4] = byte(zoneMinutes/10%6) + '0'
		out[i+5] = byte(zoneMinutes%10) + '0'
		i += 6
	}
	out[i] = '"'
	return i + 1
}

//go:noinline
func growTime(dst []byte) []byte {
	return slices.Grow(dst, len(time.RFC3339Nano)+2)
}
