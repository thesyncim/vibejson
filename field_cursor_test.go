package vibejson

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/thesyncim/vibejson/document"
)

type refMember struct {
	key string
	raw []byte
}

func refFields(t *testing.T, v Value) []refMember {
	t.Helper()
	members, ok := v.Object()
	if !ok {
		t.Fatalf("Object() failed on kind %v", v.Kind())
	}
	out := make([]refMember, len(members))
	for i, m := range members {
		out[i] = refMember{key: m.Key, raw: append([]byte(nil), m.Value.Node().Raw().Bytes()...)}
	}
	return out
}

type refCursor struct {
	members []refMember
	pos     int
}

func (c *refCursor) find(key string) (raw []byte, ok bool) {
	n := len(c.members)
	if n == 0 {
		return nil, false
	}
	for scanned := 0; scanned < n; scanned++ {
		i := (c.pos + scanned) % n
		if c.members[i].key == key {
			c.pos = (i + 1) % n
			return c.members[i].raw, true
		}
	}
	c.pos = 0
	return nil, false
}

func checkCursorAgainstRef(t *testing.T, src []byte, keys []string) {
	t.Helper()
	v, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	members := refFields(t, v)

	ref := &refCursor{members: members}
	nodeCursor := v.Node().Fields()
	valueCursor := v.Fields()

	for step, key := range keys {
		wantRaw, wantOK := ref.find(key)

		gotNode, gotOK := nodeCursor.Find(key)
		if gotOK != wantOK {
			t.Fatalf("src=%q step %d key=%q: Node cursor ok=%v want %v", src, step, key, gotOK, wantOK)
		}
		if gotOK && !bytes.Equal(gotNode.Raw().Bytes(), wantRaw) {
			t.Fatalf("src=%q step %d key=%q: Node cursor raw=%q want %q", src, step, key, gotNode.Raw().Bytes(), wantRaw)
		}
	}

	ref = &refCursor{members: members}
	for step, key := range keys {
		wantRaw, wantOK := ref.find(key)

		gotValue, gotOK := valueCursor.Find(key)
		if gotOK != wantOK {
			t.Fatalf("src=%q step %d key=%q: Value cursor ok=%v want %v", src, step, key, gotOK, wantOK)
		}
		if gotOK && !bytes.Equal(gotValue.Node().Raw().Bytes(), wantRaw) {
			t.Fatalf("src=%q step %d key=%q: Value cursor raw=%q want %q", src, step, key, gotValue.Node().Raw().Bytes(), wantRaw)
		}
	}
}

func adversarialFieldObjects() []string {
	return []string{
		`{}`,
		`{"a":1}`,
		`{"a":1,"b":2,"c":3}`,
		`{"a":1,"b":2,"a":3}`,
		`{"a":1,"a":2,"a":3}`,
		`{"z":9,"y":8,"x":7,"w":6}`,
		`{"a":{"n":1},"b":[1,2,3],"c":{"m":{"k":4}}}`,
		`{"a":[1,{"x":2}],"b":{"y":[3,4]},"a":5}`,
		`{"aA":1,"aA":2,"b":3}`,
		`{"tab\tkey":1,"newline\nkey":2,"quote\"key":3}`,
		`{"":1,"a":2,"":3}`,
		`{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7}`,
		`{"outer":{"a":1,"a":2},"a":{"a":3}}`,
	}
}

func objectKeys(t *testing.T, src []byte) []string {
	t.Helper()
	v, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	members, ok := v.Object()
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range members {
		if !seen[m.Key] {
			seen[m.Key] = true
			out = append(out, m.Key)
		}
	}
	return out
}

func TestFieldCursorMatchesReference(t *testing.T) {
	for _, src := range adversarialFieldObjects() {
		keys := objectKeys(t, []byte(src))

		checkCursorAgainstRef(t, []byte(src), keys)

		reversed := make([]string, len(keys))
		for i := range keys {
			reversed[i] = keys[len(keys)-1-i]
		}
		checkCursorAgainstRef(t, []byte(src), reversed)

		var repeated []string
		for _, k := range keys {
			repeated = append(repeated, k, k, k)
		}
		checkCursorAgainstRef(t, []byte(src), repeated)

		var mixed []string
		for _, k := range keys {
			mixed = append(mixed, "__absent__", k, "missing", k)
		}
		mixed = append(mixed, "still-missing")
		checkCursorAgainstRef(t, []byte(src), mixed)

		afterMiss := append([]string{"nope"}, keys...)
		checkCursorAgainstRef(t, []byte(src), afterMiss)
	}
}

func TestFieldCursorSweepMatchesGetFirstOccurrence(t *testing.T) {
	for _, src := range adversarialFieldObjects() {
		v, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		members, ok := v.Object()
		if !ok {
			continue
		}

		firstRaw := map[string][]byte{}
		var order []string
		for _, m := range members {
			if _, seen := firstRaw[m.Key]; !seen {
				firstRaw[m.Key] = append([]byte(nil), m.Value.Node().Raw().Bytes()...)
				order = append(order, m.Key)
			}
		}

		cursor := v.Fields()
		for _, key := range order {
			got, ok := cursor.Find(key)
			if !ok {
				t.Fatalf("src=%q sweep: cursor missed key %q", src, key)
			}
			if !bytes.Equal(got.Node().Raw().Bytes(), firstRaw[key]) {
				t.Fatalf("src=%q sweep key=%q: cursor got %q want first occurrence %q",
					src, key, got.Node().Raw().Bytes(), firstRaw[key])
			}
		}
	}
}

func TestFieldCursorNonObject(t *testing.T) {
	for _, src := range []string{`123`, `"s"`, `true`, `null`, `[1,2,3]`} {
		v, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		nc := v.Node().Fields()
		if _, ok := nc.Find("a"); ok {
			t.Fatalf("src=%q: Node cursor found a key in a non-object", src)
		}
		vc := v.Fields()
		if _, ok := vc.Find("a"); ok {
			t.Fatalf("src=%q: Value cursor found a key in a non-object", src)
		}
	}
	var zero FieldCursor
	if _, ok := zero.Find("a"); ok {
		t.Fatal("zero FieldCursor found a key")
	}
	var zeroValue ValueFieldCursor
	if _, ok := zeroValue.Find("a"); ok {
		t.Fatal("zero ValueFieldCursor found a key")
	}
}

func TestFieldCursorZeroAlloc(t *testing.T) {
	for _, src := range []string{
		`{"a":1,"b":2,"c":3}`,
		`{"a":{"n":1},"b":[1,2,3],"c":4}`,
	} {
		v, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		cursor := v.Node().Fields()
		if allocs := testing.AllocsPerRun(1000, func() {
			cursor.Find("b")
			cursor.Find("absent")
			cursor.Find("a")
		}); allocs != 0 {
			t.Fatalf("src=%q: Find allocated %v times per run, want 0", src, allocs)
		}
	}
}

func TestFieldCursorRepeatedWrap(t *testing.T) {
	src := []byte(`{"a":1,"b":2,"a":3,"b":4,"a":5}`)
	v, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	members := refFields(t, v)
	ref := &refCursor{members: members}
	cursor := v.Node().Fields()

	var keys []string
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			keys = append(keys, "a")
		} else {
			keys = append(keys, "b")
		}
	}
	for step, key := range keys {
		wantRaw, wantOK := ref.find(key)
		gotNode, gotOK := cursor.Find(key)
		if gotOK != wantOK {
			t.Fatalf("step %d key=%q: ok=%v want %v", step, key, gotOK, wantOK)
		}
		if gotOK && !bytes.Equal(gotNode.Raw().Bytes(), wantRaw) {
			t.Fatalf("step %d key=%q: raw=%q want %q", step, key, gotNode.Raw().Bytes(), wantRaw)
		}
	}
}

func buildWideObject(n int) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 0; i < n; i++ {
		if i != 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"k`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":`)
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteByte('}')
	return b.Bytes()
}

func TestFieldCursorFlatWide(t *testing.T) {
	src := buildWideObject(32)
	keys := objectKeys(t, src)
	checkCursorAgainstRef(t, src, keys)

	shuffled := make([]string, len(keys))
	for i := range keys {
		shuffled[i] = keys[(i*7)%len(keys)]
	}
	checkCursorAgainstRef(t, src, shuffled)
}

var citmFieldOrder = []string{"id", "start", "price", "seats", "name", "soldOut", "sections"}

func readEventCursor(ev Value) float64 {
	c := ev.Fields()
	var s float64
	for _, key := range citmFieldOrder {
		if f, ok := c.Find(key); ok {
			s += fieldScalar(f)
		}
	}
	return s
}

func readEventGet(ev Value) float64 {
	var s float64
	for _, key := range citmFieldOrder {
		if f, ok := ev.Get(key); ok {
			s += fieldScalar(f)
		}
	}
	return s
}

func fieldScalar(v Value) float64 {
	switch v.Kind() {
	case document.Number:
		f, _ := v.Float64()
		return f
	case document.String:
		if b, ok := v.Node().StringBytes(); ok {
			return float64(len(b))
		}
		return 0
	case document.Bool:
		if b, _ := v.Bool(); b {
			return 1
		}
		return 0
	default:
		return 1
	}
}

func BenchmarkFieldCursorCitm(b *testing.B) {
	citm := citmLikeJSON(1024)
	v, err := Parse(citm)
	if err != nil {
		b.Fatal(err)
	}
	events, ok := v.Get("events")
	if !ok {
		b.Fatal("events missing")
	}
	eventList, ok := events.Array()
	if !ok {
		b.Fatal("events not an array")
	}

	b.Run("Cursor", func(b *testing.B) {
		b.ReportAllocs()
		var s float64
		for range b.N {
			for _, ev := range eventList {
				s += readEventCursor(ev)
			}
		}
		lazyFloatSink = s
	})
	b.Run("Get", func(b *testing.B) {
		b.ReportAllocs()
		var s float64
		for range b.N {
			for _, ev := range eventList {
				s += readEventGet(ev)
			}
		}
		lazyFloatSink = s
	})
}
