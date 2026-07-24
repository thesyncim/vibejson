# Migrating to slopjson

The repository, module, and root package now use the `slopjson` identity. The
rename is a pre-v1 breaking change; no forwarding module is maintained.

Replace this former module path:

```text
github.com/thesyncim/simdjson
```

with:

```text
github.com/thesyncim/slopjson
```

Then update the module graph:

```sh
go get github.com/thesyncim/slopjson@latest
go mod tidy
```

Default imports now use `slopjson.X` selectors. A temporary explicit local alias
can stage source migration without changing behavior:

```go
import oldjson "github.com/thesyncim/slopjson"
```

Repository-specific build tags, environment variables, and artifacts use the
`slopjson` or `SLOPJSON_` spelling. Public type names that describe SIMD as a
technology, such as `MarshalerSimd`, are unchanged.

Store and FileStore formats do not encode the module or package name. The rename
does not require rewriting data files. Format compatibility is governed by
version and checksum validation described in [docs/store.md](docs/store.md).
