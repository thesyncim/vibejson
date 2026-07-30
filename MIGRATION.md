# Migrating to vibejson

The repository, module, and root package now use the `vibejson` identity. The
rename was a pre-v1 breaking change; no forwarding module is maintained.

This guide covers source migration only. It does not imply API stability or a
compatibility window.

## Module path

Replace:

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
go test ./...
```

Default imports now use `vibejson` selectors:

```go
import "github.com/thesyncim/vibejson"

if !vibejson.Valid(data) {
	return errors.New("invalid JSON")
}
```

An explicit alias can stage a large source migration without changing every
selector in the same commit:

```go
import oldjson "github.com/thesyncim/vibejson"
```

Remove the alias after the call sites have moved to the new package name.

## Native hook names

Types using the pre-v1 native hook interfaces must rename their methods:

| Old identity | Current identity |
| --- | --- |
| `MarshalSimdJSON` | `MarshalVibeJSON` |
| `UnmarshalSimdJSON` | `UnmarshalVibeJSON` |
| `simdjson_validate_hooks` build tag | `vibejson_validate_hooks` |
| SIMD-named public types | `MarshalerSimd`, `UnmarshalerSimd` |

The `MarshalerSimd` and `UnmarshalerSimd` type names describe the native codec
surface and remain unchanged. Re-run interface assertions and tests after
renaming methods.

Repository-specific build tags, environment variables, cache directories, and
artifacts now use `vibejson` or `VIBEJSON_` spelling.

## Database packages

Collections, persistence, queries, SQL, and protocol packages moved to
[vibedb](https://github.com/thesyncim/vibedb).

Imports of the former `store`, `store/durable`, `query`, `sql`, `vibesql`, and
`pgwire` packages should change from:

```text
github.com/thesyncim/vibejson/<package>
```

to:

```text
github.com/thesyncim/vibedb/<package>
```

Database API, storage-format, and deployment migration guidance belongs to the
vibedb repository. This module now contains only:

- the root JSON API;
- the `document` and `simd` support packages;
- the unstable shared `x/` packages;
- repository tooling, corpora, and benchmarks.

## Migration checklist

1. update the module path and imports;
2. rename native hook methods;
3. move database-package imports to `vibedb`;
4. replace repository-specific build tags and environment names;
5. run `go mod tidy` and inspect the resulting module graph;
6. run typed-codec, stream, and zero-copy lifetime tests; and
7. review the current pre-v1 API documentation before deploying.
