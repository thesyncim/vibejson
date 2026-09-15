package simd

import (
	"bytes"
	"testing"
	"time"
)

var timeDigitsSink [20]byte

// xorshift is the deterministic generator shared by these time tests: a
// fixed-seed xorshift64 so a failing case reproduces on every run, matching the
// pattern the package's other differential tests use.
type xorshift struct{ state uint64 }

func newXorshift() *xorshift { return &xorshift{state: 0x9e3779b97f4a7c15} }

func (r *xorshift) next() uint64 {
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return r.state
}

// intN returns a deterministic value in [0, n), mirroring rand.IntN.
func (r *xorshift) intN(n int) int { return int(r.next() % uint64(n)) }

func TestAppendTimeMatchesTime(t *testing.T) {
	locations := []*time.Location{
		time.UTC,
		time.FixedZone("west", -5*60*60-30*60),
		time.FixedZone("east", 14*60*60+45*60),
		time.FixedZone("positive sub-minute", 30),
		time.FixedZone("negative sub-minute", -30),
		time.FixedZone("positive boundary", 24*60*60-1),
		time.FixedZone("negative boundary", -24*60*60+1),
	}
	rng := newXorshift()
	for _, location := range locations {
		for range 100_000 {
			year := rng.intN(10_000)
			month := time.Month(rng.intN(12) + 1)
			day := rng.intN(28) + 1
			hour := rng.intN(24)
			minute := rng.intN(60)
			second := rng.intN(60)
			nanosecond := rng.intN(1_000_000_000)
			value := time.Date(year, month, day, hour, minute, second, nanosecond, location)

			want := []byte{'"'}
			var err error
			want, err = value.AppendText(want)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '"')
			got, err := AppendTime([]byte("prefix"), value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, append([]byte("prefix"), want...)) {
				t.Fatalf("AppendTime(%v) = %q, want %q", value, got, want)
			}
		}
	}
}

// TestAppendTimeCachedMatchesTime drives one shared cache through clustered
// sequences — repeated seconds, same-day steps, day rollovers, zone flips,
// varied fractions, and interleaved errors — and checks every append against
// AppendText plus the cache-off path.
func TestAppendTimeCachedMatchesTime(t *testing.T) {
	locations := []*time.Location{
		time.UTC,
		time.FixedZone("west", -5*60*60-30*60),
		time.FixedZone("east", 14*60*60+45*60),
	}
	var cache TimeCache
	rng := newXorshift()
	base := time.Date(2026, time.July, 14, 9, 30, 0, 0, time.UTC)
	step := base
	for i := range 200_000 {
		var value time.Time
		switch i % 8 {
		case 0, 1:
			value = step // repeated second, varying fraction below
		case 2, 3:
			step = step.Add(time.Duration(rng.intN(90)) * time.Second)
			value = step
		case 4:
			step = step.Add(time.Duration(rng.intN(48)) * time.Hour)
			value = step
		case 5:
			value = step.In(locations[rng.intN(len(locations))])
		case 6:
			value = time.Date(rng.intN(10_000), time.Month(rng.intN(12)+1), rng.intN(28)+1,
				rng.intN(24), rng.intN(60), rng.intN(60), 0, locations[rng.intN(len(locations))])
		default:
			// Out-of-range years must error identically and leave the
			// cache consistent for the next valid append.
			value = time.Date(-rng.intN(3)-1, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if rng.intN(2) == 0 {
			value = value.Add(time.Duration(rng.intN(1_000_000_000)) * time.Nanosecond)
			step = value
		}

		want, wantErr := AppendTime([]byte("p"), value)
		got, gotErr := AppendTimeCached([]byte("p"), value, &cache)
		if (wantErr != nil) != (gotErr != nil) {
			t.Fatalf("step %d: AppendTimeCached(%v) err = %v, want %v", i, value, gotErr, wantErr)
		}
		if wantErr != nil {
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("step %d: AppendTimeCached(%v) = %q, want %q", i, value, got, want)
		}
	}
}

// TestAbsDaysToDateExhaustive checks the ported civil-date computation
// against the standard library for every day AppendTime can emit, plus the
// rejected years bordering the supported range.
func TestAbsDaysToDateExhaustive(t *testing.T) {
	start := time.Date(-1, time.January, 1, 12, 0, 0, 0, time.UTC)
	for day := range (10_001 + 2) * 366 {
		value := start.AddDate(0, 0, day)
		wantYear, wantMonth, wantDay := value.Date()
		if wantYear > 10_000 {
			break
		}
		abs := uint64(value.Unix() + unixToAbsolute)
		year, month, mday := absDaysToDate(abs / secondsPerDay)
		if year != wantYear || time.Month(month) != wantMonth || int(mday) != wantDay {
			t.Fatalf("absDaysToDate(day %d) = %d-%d-%d, want %d-%d-%d",
				day, year, month, mday, wantYear, int(wantMonth), wantDay)
		}
	}
}

func TestAppendTimeErrorsDoNotChangeDestination(t *testing.T) {
	for _, value := range []time.Time{
		time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(10_000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("invalid", 24*60*60)),
	} {
		storage := bytes.Repeat([]byte{0xa5}, 64)
		copy(storage, "prefix")
		before := append([]byte(nil), storage...)
		dst := storage[:len("prefix"):48]
		got, err := AppendTime(dst, value)
		if err == nil {
			t.Fatalf("AppendTime(%v) succeeded", value)
		}
		if string(got) != "prefix" {
			t.Fatalf("AppendTime changed destination to %q", got)
		}
		if !bytes.Equal(storage, before) {
			t.Fatalf("AppendTime(%v) modified destination backing", value)
		}
	}
}

func TestAppendTimeRespectsDestinationBounds(t *testing.T) {
	values := []time.Time{
		time.Date(0, 1, 2, 3, 4, 5, 0, time.UTC),
		time.Date(2026, 7, 13, 14, 37, 52, 123_456_700, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.FixedZone("east", 14*60*60+45*60)),
		time.Date(2026, 1, 1, 0, 0, 0, 1, time.FixedZone("sub-minute", -30)),
	}
	for _, value := range values {
		want := []byte("pre:\"")
		var err error
		want, err = value.AppendText(want)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, '"')
		checkAppendDestinationBounds(t, "AppendTime", value, want, func(dst []byte) ([]byte, bool) {
			got, err := AppendTime(dst, value)
			return got, err == nil
		})
	}
}

func BenchmarkAppendTime(b *testing.B) {
	value := time.Date(2026, 7, 13, 14, 37, 52, 123_456_700, time.FixedZone("west", -4*60*60))
	b.Run("simd", func(b *testing.B) {
		buf := make([]byte, 0, 64)
		for b.Loop() {
			buf, _ = AppendTime(buf[:0], value)
		}
	})
	b.Run("time", func(b *testing.B) {
		buf := make([]byte, 0, 64)
		for b.Loop() {
			buf = append(buf[:0], '"')
			buf, _ = value.AppendText(buf)
			buf = append(buf, '"')
		}
	})
}

func BenchmarkStoreDateTimeDigits(b *testing.B) {
	b.Run("selected", func(b *testing.B) {
		var dst [20]byte
		for b.Loop() {
			storeDateTimeParts(&dst, 2026, 7, 13, 14, 37, 52)
		}
		timeDigitsSink = dst
	})
	b.Run("scalar", func(b *testing.B) {
		var dst [20]byte
		for b.Loop() {
			storeDateTimePartsScalar(&dst, 2026, 7, 13, 14, 37, 52)
		}
		timeDigitsSink = dst
	})
}
