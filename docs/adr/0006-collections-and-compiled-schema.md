# ADR 0006: JSON collections and compiled schema contracts

Status: accepted for the pre-v1 Store surface.

## Context

`Store` already has the physical properties expected of one JSON collection:
an independent keyspace, immutable snapshot sequence, TTL directory, index
catalog, and writer serialization boundary. Adding a relational table number
to every row would duplicate that boundary, enlarge stable-slot metadata, and
make all point reads pay for a namespace decision that callers normally make
once.

Optional schema enforcement must cover every ingestion route without parsing a
document twice or turning schemaless writes into a larger per-generation
allocation. It must also survive recovery. Enforcing a schema only in
`Store.Put`, or forgetting it when a heap checkpoint becomes a `FileStore`,
would make the contract dependent on the API used to write the same collection.

## Decision

### A Store is one physical collection

`Collection` is a named handle embedding one `Store`. `Database` is a
concurrency-safe in-memory catalog of those handles:

- catalog create, lookup, unlink, and listing take the catalog lock;
- collection creation validates and freezes Store options before catalog
  publication;
- after lookup, CRUD, snapshots, TTL, indexes, and queries use the ordinary
  Store path and never consult the catalog;
- no row stores a collection id, catalog pointer, or schema pointer;
- two collections may own the same key without coordination;
- `DropCollection` unlinks a name. Handles and snapshots acquired earlier
  remain valid, like an unlinked file, and a later collection may reuse the
  name without aliasing the old graph.

The catalog is intentionally not a second storage engine. A `FileStore`,
Store checkpoint, or page file currently represents one durable collection.
Persisting one atomic multi-collection catalog is separate work; the current
API does not claim cross-collection transactions.

### Schemas are compiled, immutable collection configuration

`CompileStoreSchema` owns and compiles a root constraint plus sorted RFC 6901
field constraints. A field has an accepted JSON-type mask and an independent
`Required` bit. Required distinguishes absence from a present `null`; callers
must include `SchemaNull` to accept the latter. Paths may address nested object
members or array elements. `SchemaInteger` means the JSON number spelling has
no fraction or exponent and does not imply a machine-width range.

Unspecified fields are allowed. This is an evolving-document contract, not an
implementation of JSON Schema and not a closed relational row definition.
Changing a schema in place is not supported: create or bulk-rewrite a
collection with the new compiled contract so every existing row is checked.

The compiled schema is safe for concurrent reuse. Its successful
`ValidateIndex` path allocates no memory. Canonical field order produces one
stable identity independent of declaration order. Redundant
`SchemaNumber | SchemaInteger` masks canonicalize to `SchemaNumber` because
integer is a number subtype.

### Validation is fused with existing work

The authoritative rule is “validate before publication”:

| Flow | Validation boundary |
| --- | --- |
| `Store.Put` | after the single structural-index build, before shape-tape compaction and state publication |
| `StoreBuilder.Append` | the same parse/validate/compact sequence before consuming the key or slot |
| `FileStore.Put` | the mutation's existing structural parse, before page allocation or staging |
| `Store.WriteFileStore` | one page-local gather per constrained path across up to 64 source rows, before the write plan |
| `Store.WriteTo` / `OpenStore` | definition is persisted; open recompiles it and validates page batches before publishing the graph |
| page file / `StorePageDB` | root hash binds the caller-supplied schema; `Put` validates under the writer transaction before page work |

A failed constraint changes no key, row, generation, TTL, index, or durable
file extent. Failure diagnostics allocate as needed; success remains the hot
path.

Shape tapes require validation before compaction because compact rows may omit
the full key tape. Bulk and recovery paths already own compact rows, so they use
`DocSet.AppendPointerRows` once per field over a complete micro-page. This
preserves template/shape fast paths and avoids widening compact tapes.

### Schemaless state retains its previous physical layout

The schema pointer lives once in the collection configuration. Immutable
`storeState` generations carry only the original pointer-free option subset.
Schema-on and schema-off chunk builders are specialized: the schemaless
builder still calls `DocSet.Append` directly, while the schema builder inserts
validation between parse and compaction. The small duplicated choreography is
intentional; a callback or inner-loop mode branch would penalize every
schemaless mutation.
Allocation-contract tests cover validation over an already-built index, and
mutation tests pin the specialized schema-on and schema-off routes.

## Durability and mismatch behavior

Heap Store image version 2 serializes the canonical schema definition in the
checksummed manifest. `OpenStore` recompiles it and fails closed if the
definition is malformed or any restored row violates it.

`FileStore` and the bounded page-file format bind the schema identity into the
durable catalog hash and set an explicit root option bit. Their open options
must provide the same compiled schema; nil or a different canonical constraint
fails before the store is returned, while declaration order alone does not
change the identity. `FileStore` keeps bounded recovery and does not scan the
corpus on open: committed document pages remain protected by their framing and
CRC32C, and every new mutation is validated before commit.

The formats are pre-v1 and intentionally changed rather than preserving a
schema-blind compatibility path.

## Consequences

- Nested and compound indexes work unchanged inside schema-bound collections;
  schema enforcement does not introduce a second query representation.
- Held collection handles have the same read/write cost as `Store`.
- There is one schema object per configured collection, not one pointer per
  row or immutable generation.
- Cross-collection transactions, a durable collection catalog, closed schemas,
  defaults, coercion, and in-place schema migration are not claimed.
