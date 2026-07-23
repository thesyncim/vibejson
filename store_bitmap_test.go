package simdjson

import (
	"fmt"
	"slices"
	"testing"
)

func TestStoreDenseBitmapBooleanDifferential(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range []StoreIndexDefinition{
		{Name: "country", Paths: []string{"/profile/country"}},
		{Name: "active", Paths: []string{"/active"}},
		{Name: "tier", Paths: []string{"/tier"}},
	} {
		if err := builder.CreateIndex(def); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 137; i++ {
		doc := fmt.Sprintf(`{"profile":{"country":"%s"},"active":%t,"tier":%d}`,
			[]string{"PT", "US", "DE"}[i%3], i%2 == 0, i%5)
		if err := builder.Append(fmt.Sprintf("key:%03d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	pt := testScalarIndex(t, `"PT"`)
	active := testScalarIndex(t, `true`)
	tier := testScalarIndex(t, `2`)

	words := snapshot.StoreBitmapWords()
	storage := make([]uint64, 0, words*4)
	storage, err = snapshot.AppendIndexBitmap(storage, "country", pt)
	if err != nil {
		t.Fatal(err)
	}
	countryWords := storage[:words]
	storage, err = snapshot.AppendIndexBitmap(storage, "active", active)
	if err != nil {
		t.Fatal(err)
	}
	activeWords := storage[words : 2*words]
	storage, err = snapshot.AppendIndexBitmap(storage, "tier", tier)
	if err != nil {
		t.Fatal(err)
	}
	tierWords := storage[2*words : 3*words]
	storage = snapshot.AppendLiveBitmap(storage)
	liveWords := storage[3*words:]

	and := AppendStoreBitmapAnd(nil, countryWords, activeWords)
	and3 := AppendStoreBitmapAnd3(nil, countryWords, activeWords, tierWords)
	notCountry := AppendStoreBitmapAndNot(nil, liveWords, countryWords)
	all := AppendStoreBitmapOr(nil, countryWords, notCountry)

	var wantAnd, wantAnd3, wantNot []string
	for i := 0; i < 137; i++ {
		key := fmt.Sprintf("key:%03d", i)
		isPT, isActive, isTier := i%3 == 0, i%2 == 0, i%5 == 2
		if isPT && isActive {
			wantAnd = append(wantAnd, key)
		}
		if isPT && isActive && isTier {
			wantAnd3 = append(wantAnd3, key)
		}
		if !isPT {
			wantNot = append(wantNot, key)
		}
	}
	if got := snapshot.AppendBitmapKeys(nil, and); !slices.Equal(got, wantAnd) {
		t.Fatalf("AND keys = %v, want %v", got, wantAnd)
	}
	if got := snapshot.AppendBitmapKeys(nil, and3); !slices.Equal(got, wantAnd3) {
		t.Fatalf("AND3 keys = %v, want %v", got, wantAnd3)
	}
	if got := snapshot.AppendBitmapKeys(nil, notCountry); !slices.Equal(got, wantNot) {
		t.Fatalf("NOT keys = %v, want %v", got, wantNot)
	}
	if got := snapshot.AppendBitmapRows(nil, all); len(got) != snapshot.Len() {
		t.Fatalf("OR universe rows = %d, want %d", len(got), snapshot.Len())
	}
}

func TestStoreDenseBitmapSteadyAllocs(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 64; i++ {
		if _, err := store.Put(fmt.Sprintf("k:%02d", i), []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.CreateIndex(StoreIndexDefinition{Name: "v", Paths: []string{"/v"}})
	if err != nil {
		t.Fatal(err)
	}
	for info.State != StoreIndexReady {
		info, err = store.BackfillIndex("v", 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.Snapshot()
	value := testScalarIndex(t, `1`)
	words := snapshot.StoreBitmapWords()
	a, b, out := make([]uint64, 0, words), make([]uint64, 0, words), make([]uint64, 0, words)
	rows := make([]StoreRow, 0, snapshot.Len())
	allocs := testing.AllocsPerRun(100, func() {
		var runErr error
		a, runErr = snapshot.AppendIndexBitmap(a[:0], "v", value)
		if runErr != nil {
			panic(runErr)
		}
		b = snapshot.AppendLiveBitmap(b[:0])
		out = AppendStoreBitmapAnd(out[:0], a, b)
		rows = snapshot.AppendBitmapRows(rows[:0], out)
	})
	if allocs != 0 || len(rows) != snapshot.Len() {
		t.Fatalf("bitmap pipeline = %.2f allocs, %d rows", allocs, len(rows))
	}
}
