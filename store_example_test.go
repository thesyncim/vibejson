package slopjson_test

import (
	"bytes"
	"fmt"
	"time"

	"github.com/thesyncim/slopjson"
)

func ExampleStore() {
	var store slopjson.Store
	_, _ = store.Put("user:42", []byte(`{"name":"Ada","score":7}`))
	before := store.Snapshot()

	created, _ := store.Put("user:42", []byte(`{"name":"Ada","score":8}`))
	current, _ := store.GetRaw("user:42")
	old, _ := before.GetRaw("user:42")
	fmt.Printf("created=%v current=%s old=%s\n", created, current.Bytes(), old.Bytes())

	// Output:
	// created=false current={"name":"Ada","score":8} old={"name":"Ada","score":7}
}

func ExampleStoreBuilder() {
	builder, _ := slopjson.NewStoreBuilder(slopjson.StoreOptions{ShapeTapes: true})
	_ = builder.CreateIndex(slopjson.StoreIndexDefinition{
		Name: "country", Paths: []string{"/profile/country"},
	})
	_ = builder.Append("user:1", []byte(`{"profile":{"country":"PT"}}`))
	_ = builder.Append("user:2", []byte(`{"profile":{"country":"US"}}`))
	store, _ := builder.Build()

	keys, _ := store.Snapshot().AppendIndexRawKeys(nil, "country", []byte(`"PT"`))
	fmt.Println(store.Generation(), keys)

	// Output:
	// 1 [user:1]
}

func ExampleOpenStore() {
	var original slopjson.Store
	_, _ = original.Put("user:42", []byte(`{"name":"Ada"}`))

	var image bytes.Buffer
	_, _ = original.WriteTo(&image)
	reopened, _ := slopjson.OpenStore(image.Bytes())

	dst := make([]byte, 0, 32)
	dst, ok := reopened.AppendRaw(dst, "user:42")
	fmt.Printf("%v %s\n", ok, dst)

	// Output:
	// true {"name":"Ada"}
}

func ExampleStore_SetDeadline() {
	var store slopjson.Store
	_, _ = store.Put("session", []byte(`{"user":42}`))
	deadline := time.Now().Add(time.Hour)
	store.SetDeadline("session", deadline)
	before := store.Snapshot()

	fmt.Println(store.ExpireDue(deadline.Add(time.Second), 0))
	_, current := store.GetRaw("session")
	_, old := before.GetRaw("session")
	fmt.Println(current, old)

	// Output:
	// 1
	// false true
}

func ExampleStore_AddIndex() {
	store := slopjson.NewStore(slopjson.StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	_, _ = store.Put("a", []byte(`{"team":"compiler"}`))
	_, _ = store.Put("b", []byte(`{"team":"runtime"}`))
	_, _ = store.Put("c", []byte(`{"team":"compiler"}`))

	info, _ := store.AddIndex("team-search", slopjson.StoreIndexPostings)
	for info.State != slopjson.StoreIndexReady {
		info, _ = store.BackfillIndex("team-search", 1)
	}

	src := []byte(`"compiler"`)
	need, _ := slopjson.RequiredIndexEntries(src)
	needle, _ := slopjson.BuildIndex(src, make([]slopjson.IndexEntry, 0, need))
	keys := store.AppendWhereContainsIndexKeys(make([]string, 0, store.Len()), "team", needle)
	fmt.Println(keys)

	// Output:
	// [a c]
}
