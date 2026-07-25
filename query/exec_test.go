package query

import (
	"fmt"
	"sync"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

// TestZeroSourceRejected pins the discriminated union's invalid zero state: a
// Source that was declared but never constructed names no collection, and must
// fail rather than report an empty result from an empty DocSet.
func TestZeroSourceRejected(t *testing.T) {
	q := Select(Count())
	var e Exec
	if err := q.RunInto(&e, Source{}); err == nil {
		t.Fatal("zero Source executed")
	}
	if _, err := q.Run(Source{}); err == nil {
		t.Fatal("zero Source ran")
	}
	if err := q.RunInto(nil, FromDocSet(&store.DocSet{})); err == nil {
		t.Fatal("nil Exec executed")
	}
	if err := q.RunInto(&e, FromDocSet(nil)); err == nil {
		t.Fatal("nil DocSet executed")
	}
}

// TestConcurrentExecsShareOneQuery pins the concurrency contract the single
// cached compiled plan rests on: one Query is immutable after its sync.Once
// compiles it, and everything an execution mutates lives in the caller's Exec.
// Each goroutine therefore needs only its own Exec, and the race detector is
// the oracle for the claim.
func TestConcurrentExecsShareOneQuery(t *testing.T) {
	docs := make([][]byte, 256)
	for i := range docs {
		docs[i] = fmt.Appendf(nil, `{"id":%d,"bucket":%d,"name":"item-%d"}`, i, i%8, i)
	}
	set := buildDocSet(t, docs, storageMode{"", true, true})
	q := Select(Path("bucket"), Count()).GroupBy("bucket").OrderBy("bucket", Asc)
	want := resultKey(mustRun(t, q, set))

	var wg sync.WaitGroup
	keys := make([]string, 8)
	for g := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var e Exec
			src := FromDocSet(set)
			for range 16 {
				if err := q.RunInto(&e, src); err != nil {
					panic(err)
				}
			}
			keys[g] = resultKey(e.Result)
		}()
	}
	wg.Wait()
	for g, got := range keys {
		if got != want {
			t.Fatalf("goroutine %d result:\n got: %s\nwant: %s", g, got, want)
		}
	}
}
