package slopjson

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// churnHeap allocates and drops memory to encourage the collector to reuse any
// storage freed since the last GC, so a dangling interior pointer reads reused
// (corrupted) bytes rather than its stale-but-intact originals.
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

// deepDoc builds a document whose interesting leaves sit far from the root, so a
// handle to a leaf keeps a large index and source alive.
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

// bareNodeOutlivesValue keeps only the Node cursor (Value.Node()) and drops the
// Value. If the Node's raw src/entry pointers do not pin the owned backing, the
// churn corrupts the read.
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
	// Return bare Node cursors; v and src both go out of scope.
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

// arrayIterOutlivesValue keeps only an ArrayIter and drops the Value.
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

// rawOutlivesValue keeps only a RawValue and drops the Value.
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

// textStringOutlivesValue keeps only a decoded escaped string and drops the
// Value. Escaped strings decode into fresh storage; unescaped alias src.
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

// TestGCZeroCopyStaleData documents the ZeroCopy contract: mutating the
// caller's buffer changes what a held Node reads (stale data), but stays in
// bounds (memory-safe). This is a contract check, not a bug hunt.
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
	// Mutate in place, same length: memory-safe, observably stale.
	copy(src[6:], []byte("MUTATEDVALUE0"))
	got, _ := node.Text()
	if got != "MUTATEDVALUE0" {
		t.Fatalf("ZeroCopy did not alias: %q", got)
	}
	churnHeap()
	// Still in bounds after churn.
	_, _ = node.Text()
}
