package vibejson

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func churnHeap() {
	runtime.GC()
	sink := make([][]byte, 0, 4096)
	for i := 0; i < 4096; i++ {
		b := make([]byte, 4096)
		for j := range b {
			b[j] = 0xAB
		}
		sink = append(sink, b)
	}
	runtime.KeepAlive(sink)
	runtime.GC()
	runtime.GC()
}

func deepDoc(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"item-%d-payload","tags":["x","y","z"]}`, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func bareNodeOutlivesValue() (Node, Node) {
	src := deepDoc(200)
	v, err := Parse(src)
	if err != nil {
		panic(err)
	}
	items, _ := v.Get("items")
	last, _ := items.Index(150)
	name, _ := last.Get("name")
	id, _ := last.Get("id")
	return name.Node(), id.Node()
}

func TestGCBareNodeOutlivesValue(t *testing.T) {
	name, id := bareNodeOutlivesValue()
	churnHeap()
	if got, ok := name.StringBytes(); !ok || string(got) != "item-150-payload" {
		t.Fatalf("name after churn = %q ok=%v, want %q", got, ok, "item-150-payload")
	}
	if n, ok := id.Int64(); !ok || n != 150 {
		t.Fatalf("id after churn = %d ok=%v, want 150", n, ok)
	}
}

func arrayIterOutlivesValue() ArrayIter {
	src := deepDoc(200)
	v, err := Parse(src)
	if err != nil {
		panic(err)
	}
	items, _ := v.Get("items")
	it, _ := items.Node().ArrayIter()
	return it
}

func TestGCArrayIterOutlivesValue(t *testing.T) {
	it := arrayIterOutlivesValue()
	churnHeap()
	count := 0
	for {
		node, ok := it.Next()
		if !ok {
			break
		}
		id, ok := node.Get("id")
		if !ok {
			t.Fatalf("element %d missing id after churn", count)
		}
		if n, _ := id.Int64(); n != int64(count) {
			t.Fatalf("element %d id = %d after churn, want %d", count, n, count)
		}
		count++
	}
	if count != 200 {
		t.Fatalf("iterated %d elements after churn, want 200", count)
	}
}

func rawOutlivesValue() RawValue {
	src := deepDoc(200)
	v, err := Parse(src)
	if err != nil {
		panic(err)
	}
	items, _ := v.Get("items")
	elem, _ := items.Index(99)
	return elem.Node().Raw()
}

func TestGCRawOutlivesValue(t *testing.T) {
	raw := rawOutlivesValue()
	churnHeap()
	want := `{"id":99,"name":"item-99-payload","tags":["x","y","z"]}`
	if got := string(raw.Bytes()); got != want {
		t.Fatalf("raw after churn = %q, want %q", got, want)
	}
}

func textStringOutlivesValue() (string, string) {
	src := []byte(`{"unescaped":"plain-source-bytes","escaped":"tab\tandéend"}`)
	v, err := Parse(src)
	if err != nil {
		panic(err)
	}
	u, _ := v.Get("unescaped")
	e, _ := v.Get("escaped")
	ut, _ := u.Text()
	et, _ := e.Text()
	return ut, et
}

func TestGCTextStringOutlivesValue(t *testing.T) {
	u, e := textStringOutlivesValue()
	churnHeap()
	if u != "plain-source-bytes" {
		t.Fatalf("unescaped text after churn = %q", u)
	}
	if e != "tab\tandéend" {
		t.Fatalf("escaped text after churn = %q", e)
	}
}

func TestGCZeroCopyStaleData(t *testing.T) {
	src := []byte(`{"k":"originalvalue"}`)
	v, err := ParseOptions(src, Options{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := v.Get("k")
	if got, _ := node.Text(); got != "originalvalue" {
		t.Fatalf("before mutation: %q", got)
	}
	copy(src[6:], []byte("MUTATEDVALUE0"))
	got, _ := node.Text()
	if got != "MUTATEDVALUE0" {
		t.Fatalf("ZeroCopy did not alias: %q", got)
	}
	churnHeap()
	_, _ = node.Text()
}
