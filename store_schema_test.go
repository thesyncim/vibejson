package simdjson

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"
)

func testStoreSchema(t testing.TB) *StoreSchema {
	t.Helper()
	schema, err := CompileStoreSchema(StoreSchemaDefinition{
		Root: SchemaObject,
		Fields: []StoreSchemaField{
			{
				Path: "/profile/name", Types: SchemaString,
				Required: true,
			},
			{Path: "/tags/0", Types: SchemaString},
			{
				Path:  "/profile/age",
				Types: SchemaInteger | SchemaNull,
			},
			{Path: "/id", Types: SchemaInteger, Required: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestStoreSchemaCompileValidateNestedAndAllocateZero(t *testing.T) {
	schema := testStoreSchema(t)
	definition := schema.Definition()
	if got := []string{
		definition.Fields[0].Path,
		definition.Fields[1].Path,
		definition.Fields[2].Path,
		definition.Fields[3].Path,
	}; !slices.Equal(got, []string{
		"/id", "/profile/age", "/profile/name", "/tags/0",
	}) {
		t.Fatalf("canonical paths = %v", got)
	}
	slices.Reverse(definition.Fields)
	reordered, err := CompileStoreSchema(definition)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.hash != schema.hash {
		t.Fatalf(
			"declaration order changed identity: %#x != %#x",
			reordered.hash, schema.hash,
		)
	}
	redundantNumber, err := CompileStoreSchema(StoreSchemaDefinition{
		Root: SchemaNumber | SchemaInteger,
	})
	if err != nil {
		t.Fatal(err)
	}
	number, err := CompileStoreSchema(StoreSchemaDefinition{
		Root: SchemaNumber,
	})
	if err != nil {
		t.Fatal(err)
	}
	if redundantNumber.hash != number.hash ||
		redundantNumber.Definition().Root != SchemaNumber {
		t.Fatalf(
			"redundant integer subtype was not canonicalized: %#x/%s",
			redundantNumber.hash,
			redundantNumber.Definition().Root,
		)
	}

	valid := []byte(
		`{"id":7,"profile":{"name":"Ada","age":null},"tags":["go"]}`,
	)
	index, err := BuildIndex(valid, make([]IndexEntry, 128))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateIndex(index); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := schema.ValidateIndex(index); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("ValidateIndex allocated %.2f times, want 0", allocs)
	}

	for _, test := range []struct {
		name     string
		document string
		path     string
		missing  bool
	}{
		{
			name: "integer spelling", document: `{"id":7.0,"profile":{"name":"Ada"}}`,
			path: "/id",
		},
		{
			name: "nested type", document: `{"id":7,"profile":{"name":false}}`,
			path: "/profile/name",
		},
		{
			name: "required nested path", document: `{"id":7,"profile":{}}`,
			path: "/profile/name", missing: true,
		},
		{
			name: "root type", document: `[]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			index, err := BuildIndex(
				[]byte(test.document), make([]IndexEntry, 128),
			)
			if err != nil {
				t.Fatal(err)
			}
			err = schema.ValidateIndex(index)
			if !errors.Is(err, ErrStoreSchemaViolation) {
				t.Fatalf("ValidateIndex = %v", err)
			}
			var violation *SchemaViolationError
			if !errors.As(err, &violation) ||
				violation.Path != test.path ||
				violation.Missing != test.missing {
				t.Fatalf("violation = %+v", violation)
			}
		})
	}
}

func TestStoreSchemaRejectsInvalidDefinitions(t *testing.T) {
	for _, definition := range []StoreSchemaDefinition{
		{Root: SchemaType(1 << 15)},
		{Fields: []StoreSchemaField{{Path: "", Types: SchemaString}}},
		{Fields: []StoreSchemaField{{
			Path: string([]byte{0xff}), Types: SchemaString,
		}}},
		{Fields: []StoreSchemaField{{Path: "not-a-pointer", Types: SchemaString}}},
		{Fields: []StoreSchemaField{{Path: "/x"}}},
		{Fields: []StoreSchemaField{
			{Path: "/x", Types: SchemaString},
			{Path: "/x", Types: SchemaNumber},
		}},
	} {
		if _, err := CompileStoreSchema(definition); !errors.Is(
			err, ErrStoreSchemaDefinition,
		) {
			t.Fatalf("CompileStoreSchema(%+v) = %v", definition, err)
		}
	}

	store := NewStore(StoreOptions{Schema: &StoreSchema{}})
	if _, err := store.Put("x", []byte(`{}`)); !errors.Is(
		err, ErrStoreSchemaDefinition,
	) {
		t.Fatalf("uncompiled Store schema = %v", err)
	}
	if _, err := NewCollection("invalid", StoreOptions{
		Schema: &StoreSchema{},
	}); !errors.Is(err, ErrStoreSchemaDefinition) {
		t.Fatalf("uncompiled Collection schema = %v", err)
	}
}

func TestStoreSchemaMutationBuilderAndSnapshotAtomicity(t *testing.T) {
	schema := testStoreSchema(t)
	options := StoreOptions{
		ChunkDocuments: 4, ShapeTapes: true, Schema: schema,
	}
	store := NewStore(options)
	oldDocument := `{"id":1,"profile":{"name":"old","age":20}}`
	if created, err := store.Put("key", []byte(oldDocument)); err != nil ||
		!created {
		t.Fatalf("initial Put = (%v,%v)", created, err)
	}
	old := store.Snapshot()
	generation := store.Generation()
	if _, err := store.Put(
		"key", []byte(`{"id":1,"profile":{"name":9}}`),
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("invalid replacement = %v", err)
	}
	if _, err := store.Put(
		"new", []byte(`{"id":2,"profile":{}}`),
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("invalid insert = %v", err)
	}
	if store.Generation() != generation || store.Len() != 1 {
		t.Fatalf(
			"rejected writes changed generation/len = %d/%d",
			store.Generation(), store.Len(),
		)
	}
	if raw, ok := store.GetRaw("key"); !ok ||
		string(raw.Bytes()) != oldDocument {
		t.Fatalf("current value after reject = (%q,%v)", raw.Bytes(), ok)
	}
	if raw, ok := old.GetRaw("key"); !ok ||
		string(raw.Bytes()) != oldDocument {
		t.Fatalf("snapshot after reject = (%q,%v)", raw.Bytes(), ok)
	}

	builder, err := NewStoreBuilder(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append(
		"bad", []byte(`{"id":1,"profile":{}}`),
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("builder invalid Append = %v", err)
	}
	if builder.Len() != 0 {
		t.Fatalf("builder consumed rejected row: %d", builder.Len())
	}
	if err := builder.Append(
		"bad", []byte(`{"id":2,"profile":{"name":"ok"}}`),
	); err != nil {
		t.Fatal(err)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if built.Options.Schema != schema || built.Len() != 1 {
		t.Fatalf("built schema/len = %p/%d", built.Options.Schema, built.Len())
	}
}

func TestDatabaseCollectionsHaveIndependentHotPaths(t *testing.T) {
	schema := testStoreSchema(t)
	var database Database
	users, err := database.CreateCollection("users", StoreOptions{
		ChunkDocuments: 8, ShapeTapes: true, Schema: schema,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := database.CreateCollection(
		"events", StoreOptions{ChunkDocuments: 8},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.Put(
		"same-key", []byte(`{"id":1,"profile":{"name":"Ada"}}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Put("same-key", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Put("bad", []byte(`[]`)); !errors.Is(
		err, ErrStoreSchemaViolation,
	) {
		t.Fatalf("users accepted invalid root: %v", err)
	}
	index, err := users.CreateIndex(StoreIndexDefinition{
		Name:  "identity",
		Paths: []string{"/id", "/profile/name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	index, err = users.BackfillIndex(index.Name, 0)
	if err != nil || index.State != StoreIndexReady {
		t.Fatalf("collection index backfill = (%+v,%v)", index, err)
	}
	keys, err := users.AppendIndexRawKeys(
		nil, "identity", []byte(`1`), []byte(`"Ada"`),
	)
	if err != nil || !slices.Equal(keys, []string{"same-key"}) {
		t.Fatalf(
			"nested compound collection index = (%v,%v)", keys, err,
		)
	}
	if raw, ok := events.GetRaw("same-key"); !ok ||
		string(raw.Bytes()) != `[]` {
		t.Fatalf("independent event key = (%q,%v)", raw.Bytes(), ok)
	}
	if _, err := database.CreateCollection("users", StoreOptions{}); !errors.Is(
		err, ErrStoreCollectionExists,
	) {
		t.Fatalf("duplicate collection = %v", err)
	}
	info := database.AppendCollections(nil)
	if len(info) != 2 || info[0].Name != "events" ||
		info[1].Name != "users" || !info[1].Schema {
		t.Fatalf("collection catalog = %+v", info)
	}
	if !database.DropCollection("users") {
		t.Fatal("DropCollection(users) missed")
	}
	if _, ok := database.Collection("users"); ok {
		t.Fatal("dropped collection remains cataloged")
	}
	// A catalog drop cannot revoke an already-held immutable graph or handle.
	if raw, ok := users.GetRaw("same-key"); !ok || len(raw.Bytes()) == 0 {
		t.Fatal("drop invalidated an existing collection handle")
	}
	replacement, err := database.CreateCollection(
		"users", StoreOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == users || replacement.Len() != 0 {
		t.Fatal("recreated name aliased the dropped collection")
	}
}

func TestStoreSchemaPersistsAndRevalidatesCheckpoint(t *testing.T) {
	schema, err := CompileStoreSchema(StoreSchemaDefinition{
		Root: SchemaObject,
		Fields: []StoreSchemaField{
			{Path: "/id", Types: SchemaInteger, Required: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(StoreOptions{
		ChunkDocuments: 2, ShapeTapes: true, Schema: schema,
	})
	document := " \n{\"id\":7}\t"
	if _, err := store.Put("key", []byte(document)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("key2", []byte(`{"id":8}`)); err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	if _, err := store.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Options.Schema == nil || !reflect.DeepEqual(
		reopened.Options.Schema.Definition(), schema.Definition(),
	) {
		t.Fatalf(
			"reopened schema = %+v",
			reopened.Options.Schema,
		)
	}
	generation := reopened.Generation()
	if _, err := reopened.Put(
		"key", []byte(`{"id":"wrong"}`),
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("reopened Store accepted invalid replacement: %v", err)
	}
	if reopened.Generation() != generation {
		t.Fatal("rejected reopened write advanced generation")
	}

	// A valid checksum is not enough: OpenStore revalidates every persisted
	// row against the restored contract before publishing the graph.
	corrupt := append([]byte(nil), image.Bytes()...)
	footer := corrupt[len(corrupt)-storePersistFooterLen:]
	offset := binary.LittleEndian.Uint64(footer[8:16])
	length := binary.LittleEndian.Uint64(footer[16:24])
	manifest := corrupt[offset : offset+length]
	field := storePersistManifestFixed
	binary.LittleEndian.PutUint16(
		manifest[field+4:field+6], uint16(SchemaString),
	)
	binary.LittleEndian.PutUint64(
		footer[24:32], persistChecksum(manifest),
	)
	if _, err := OpenStore(corrupt); !errors.Is(
		err, ErrStorePersistCorrupt,
	) {
		t.Fatalf("schema/data mismatch = %v", err)
	}

	legacy, err := os.CreateTemp(t.TempDir(), "store-page-schema-*")
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := legacy.Name()
	if _, err := store.WritePageFile(
		legacy, StorePageWriteOptions{},
	); err != nil {
		t.Fatalf("schema-bound page export = %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if reader, err := OpenStorePageReader(
		legacyPath, StorePageOpenOptions{},
	); !errors.Is(err, ErrStorePageSchemaMismatch) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("schemaless page reader = %v", err)
	}
	reader, err := OpenStorePageReader(
		legacyPath, StorePageOpenOptions{Schema: schema},
	)
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok, err := reader.AppendRaw(nil, "key"); err != nil ||
		!ok || string(raw) != document {
		t.Fatalf("schema page read = (%q,%v,%v)", raw, ok, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	pageDB, err := OpenStorePageDB(legacyPath, StorePageDBOptions{
		Open: StorePageOpenOptions{Schema: schema},
	})
	if err != nil {
		t.Fatal(err)
	}
	pageGeneration := pageDB.Generation()
	if _, err := pageDB.Put(
		"key", []byte(`{"id":"wrong"}`),
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("page DB schema rejection = %v", err)
	}
	if pageDB.Generation() != pageGeneration {
		t.Fatal("rejected page DB write advanced generation")
	}
	if _, err := pageDB.Put("key", []byte(`{"id":8}`)); err != nil {
		t.Fatal(err)
	}
	if err := pageDB.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreSchemaMutationRecoveryAndBulk(t *testing.T) {
	schema := testStoreSchema(t)
	options := testFileStoreOptions()
	options.Store.Schema = schema
	file, err := os.CreateTemp(t.TempDir(), "file-store-schema-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	store, err := CreateFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	document := []byte(`{"id":1,"profile":{"name":"Ada"}}`)
	if _, err := store.Put("key", document); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation()
	sizeBefore, err := file.Seek(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(
		"key", []byte(`{"id":1,"profile":{}}`),
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("FileStore invalid replacement = %v", err)
	}
	sizeAfter, err := file.Seek(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if store.Generation() != generation || sizeAfter != sizeBefore {
		t.Fatalf(
			"rejected FileStore write changed generation/file = %d/%d, want %d/%d",
			store.Generation(), sizeAfter, generation, sizeBefore,
		)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := reopened.AppendRaw(nil, "key"); err != nil ||
		!ok || !bytes.Equal(got, document) {
		t.Fatalf("reopened document = (%q,%v,%v)", got, ok, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	otherSchema, err := CompileStoreSchema(StoreSchemaDefinition{
		Root: SchemaArray,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrong := options
	wrong.Store.Schema = otherSchema
	if opened, err := OpenFileStore(file, wrong); err == nil {
		_ = opened.Close()
		t.Fatal("OpenFileStore accepted a mismatched schema")
	}

	source := NewStore(StoreOptions{ChunkDocuments: 2})
	if _, err := source.Put("bad", []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	bulkFile, err := os.CreateTemp(t.TempDir(), "file-store-schema-bulk-*")
	if err != nil {
		t.Fatal(err)
	}
	defer bulkFile.Close()
	if _, err := source.WriteFileStore(
		bulkFile, options,
	); !errors.Is(err, ErrStoreSchemaViolation) {
		t.Fatalf("bulk schema validation = %v", err)
	}
	if info, err := bulkFile.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("failed bulk image size = (%v,%v)", info, err)
	}

	validSource := NewStore(StoreOptions{ChunkDocuments: 2})
	if _, err := validSource.Put(
		"good", []byte(`{"id":2,"profile":{"name":"bulk"}}`),
	); err != nil {
		t.Fatal(err)
	}
	validBulk, err := os.CreateTemp(
		t.TempDir(), "file-store-schema-valid-bulk-*",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer validBulk.Close()
	if _, err := validSource.WriteFileStore(validBulk, options); err != nil {
		t.Fatal(err)
	}
	bulkStore, err := OpenFileStore(validBulk, options)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := bulkStore.AppendRaw(nil, "good"); err != nil ||
		!ok || string(got) != `{"id":2,"profile":{"name":"bulk"}}` {
		t.Fatalf("valid bulk document = (%q,%v,%v)", got, ok, err)
	}
	if err := bulkStore.Close(); err != nil {
		t.Fatal(err)
	}
}
