# Migrating to slopjson

The project, repository, root Go package, and module path now share the
`slopjson` identity. This is a pre-v1 breaking rename; no compatibility module
or forwarding package is maintained under the former identity.

Replace the module path:

```text
github.com/thesyncim/simdjson
github.com/thesyncim/slopjson
```

Then update dependencies:

```sh
go get github.com/thesyncim/slopjson@latest
go mod tidy
```

Imports that use the default package name must also change selectors from
`simdjson.X` to `slopjson.X`. A temporary explicit local import alias can make
a staged application migration compile without changing runtime behavior:

```go
import simdjson "github.com/thesyncim/slopjson"
```

Repository-scoped build tags, environment variables, temporary artifacts, and
the optional self-hosted performance runner label use the corresponding
`slopjson` or `SLOPJSON_` spelling. Public Go API type names that describe SIMD
as a technology, such as `MarshalerSimd`, are unchanged.

Persistent Store and FileStore bytes do not encode the Go module path or
package name. The rename therefore does not require rewriting existing data
files. Format compatibility remains governed by the version and checksum rules
documented in [the Store guide](docs/store.md), not by repository identity.
