# Migrating to vibejson

The repository, module, and root package now use the `vibejson` identity. The
rename is a pre-v1 breaking change; no forwarding module is maintained.

Replace this former module path:

```text
github.com/thesyncim/simdjson
```

with:

```text
github.com/thesyncim/vibejson
```

Then update the module graph:

```sh
go get github.com/thesyncim/vibejson@latest
go mod tidy
```

Default imports now use `vibejson.X` selectors. A temporary explicit local alias
can stage source migration without changing behavior:

```go
import oldjson "github.com/thesyncim/vibejson"
```

Repository-specific build tags, environment variables, and artifacts use the
`vibejson` or `VIBEJSON_` spelling. Public type names that describe SIMD as a
technology, such as `MarshalerSimd`, are unchanged. Native hook methods are now
named `MarshalVibeJSON` and `UnmarshalVibeJSON`; update method declarations on
types that opt into those interfaces.

Collection and durable-collection formats do not encode the module or package
name. The rename does not require rewriting data files. Format compatibility is
governed by version and checksum validation described in
[docs/store.md](docs/store.md).

## Collection type names

The keyed-collection types are spelled `Collection` in both storage packages.
Replace `store.Store` with `store.Collection`, `durable.Store` with
`durable.Collection`, `store.Table[S]` with `store.Mutable[S]`, and
`durable.Options.Store` with `durable.Options.Collection`. `store.NewCollection`
is removed: `store.New` returns a standalone `*store.Collection`, and
`Database.CreateCollection` is the only constructor that assigns a catalog
name. No aliases are kept; this is a pre-v1 breaking change.
