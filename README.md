# simdjson

[![ci](https://github.com/thesyncim/simdjson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/simdjson/actions/workflows/ci.yml)

Strict, high-performance JSON for Go, written entirely in Go. The ordinary
`Marshal` and `Unmarshal` APIs follow `encoding/json`; reusable typed plans,
structural indexing, and optional Go-native SIMD accelerate repeated work.
The root module has no third-party module dependencies, assembly, C,
`go:linkname`, or private runtime-layout assumptions.

This project is pre-v1: APIs may change while the accepted
[v1 boundary](docs/adr/0001-v1-api.md) is implemented. It is an independent Go
implementation, not the C++ [`simdjson`](https://github.com/simdjson/simdjson)
project. Algorithm and corpus relationships are recorded in
[the provenance inventory](docs/provenance.md).

## Install

Go 1.26 builds the supported portable backend:

```sh
go get github.com/thesyncim/simdjson@latest
```

The optional SIMD backend requires the exact Go 1.27 development compiler
pinned by the repository:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/simdjson-gotip"
GOEXPERIMENT=simd "$HOME/sdk/simdjson-gotip/bin/go" test ./...
```

`GOEXPERIMENT=simd` selects compiler support, not a CPU. On amd64,
`GOAMD64=v1` and `v2` binaries select AVX2 or the scalar fallback once during
package initialization and keep static call paths; `GOAMD64=v3` and newer
binaries require AVX2 and compile those scanner calls directly to it. ARM64
builds select the maintained NEON scanner. Go 1.28 and later deliberately use
portable source until that compiler family passes the same correctness,
escape, and performance gates. See the [toolchain policy](docs/toolchain.md).

## Typed decode

```go
package main

import "github.com/thesyncim/simdjson"

type Event struct {
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func decode(data []byte) (Event, error) {
	var event Event
	err := simdjson.Unmarshal(data, &event)
	return event, err
}
```

## Typed encode

```go
func encode(event *Event) ([]byte, error) {
	encoder, err := simdjson.CompileEncoder[Event](simdjson.EncoderOptions{})
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, 4096)
	return encoder.AppendJSON(buf, event)
}
```

`AppendJSON` reuses caller-owned capacity. `Marshal` is the allocating
convenience for occasional calls.

## Choose an API

| Need | Start with |
| --- | --- |
| Ordinary typed JSON or strict validation | `Marshal`, `Unmarshal`, `Valid`, `Validate` |
| Repeated typed work and buffer reuse | `CompileEncoder`, `CompileDecoder` |
| Framed JSON input or token output | `Reader`, `Writer` |
| Compact, indented, or canonical output | `Compact`, `Indent`, `Canonicalize` |
| Borrowed selection or repeated document navigation | `RawValue`, `Index`/`Node`, or `Parse`/`Value` |

The advanced document APIs currently remain in the root package during the
pre-v1 migration. Their target home and compatibility boundary are decided in
the [API ADR](docs/adr/0001-v1-api.md).

## Streaming input limits

`NewReader` and `NewReaderSize` do not limit the size of one JSON value. A zero
`ReaderOptions.MaxValueBytes` is also unbounded, so the rolling buffer may grow
to the largest value received. For untrusted or network input, set a positive
per-value limit chosen for the protocol before reading:

```go
reader, err := simdjson.NewReaderWithOptions(input, simdjson.ReaderOptions{
	MaxValueBytes: maxValueBytes, // positive; limit for one top-level value
})
```

Handle the constructor error before using `reader`. If a value exceeds the
limit, iteration stops and `reader.Err()` reports the error. The limit applies
to each top-level value, not to total stream size.

## Ownership and concurrency

Default typed decoding and default `Parse` own the string storage they expose.
`ZeroCopy`, `RawValue`, `Index`, `Node`, and reader cursors borrow storage: keep
the source alive and unmodified, and observe each API's invalidation rule.
`Index` and `Node` also borrow caller-provided entry storage.

Compiled encoders, decoders, and pointers are immutable and safe for concurrent
use. Destinations and source buffers remain caller-owned; each `Reader` or
`Writer` belongs to one goroutine. The complete rules are in
[ownership and lifetimes](docs/design/ownership.md).

## Performance

Performance changes must preserve correctness, ownership, retained memory,
`B/op`, and `allocs/op`. Native CI exercises matched portable and SIMD behavior
on amd64 and ARM64. The current accelerated scanner choices are AVX2 on capable
amd64 CPUs and NEON on ARM64; other CPUs use portable Go.

The repository keeps the normalized measurements in
[`benchmarks/results/latest.json`](benchmarks/results/latest.json), not a
floating leaderboard in this README. The [benchmark contract and reproduction
commands](benchmarks/README.md) explain the isolated-process methodology,
performance gates, comparison boundaries, and pinned toolchains.

## Support and project records

- [Toolchain and compiler support](docs/toolchain.md)
- [Maintenance baseline](BASELINE.md)
- [Test contract matrix](TEST_CONTRACTS.md)
- [Resource and input limits](docs/contracts/limits.md)
- [Unsafe inventory](UNSAFE.md)
- [Architecture and ownership records](docs/design/ownership.md)
- [Security policy](SECURITY.md)
- [Contributing and local gates](CONTRIBUTING.md)

The repository does not yet have a root project license. `LICENSE-GO` covers
identified Go-derived files only. Selecting a project license and completing
the final notice and attribution audit remain release blockers; no license is
implied by this README.
