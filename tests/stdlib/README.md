# Go standard library corpus

This nested module runs slopjson against every high-level payload in the pinned
Go revision's `encoding/json/internal/jsontest` corpus. It stays separate so
the root module remains dependency-free; `klauspost/compress` is used only here
to read the checked-in Zstandard assets.

## Provenance

- Repository: <https://go.googlesource.com/go>
- Commit: `03845e30f7b73d1703bd8c21017297f6eecb76d6`
- Payloads: `src/encoding/json/internal/jsontest/_embed/*.json.zst`
- Models: `src/encoding/json/internal/jsontest/testdata.go`

The seven payloads are copied byte-for-byte and the concrete models are
mechanically extracted from that revision. The corpus and metadata use Go's
BSD license; its exact text is in `testdata/LICENSE`.

Refresh and verify the copies with the pinned compiler:

```sh
./scripts/update-stdlib-corpus.sh /path/to/gotip/bin/go
./scripts/check-stdlib-corpus.sh /path/to/gotip/bin/go
```

The suite differentially checks validation, dynamic decoding, `UseNumber`,
dynamic encoding, indexed parsing, JSON reconstruction, and decoding/encoding
through the corpus's exact concrete Go models against `encoding/json`. Owned
and zero-copy decoding are checked and benchmarked as separate contracts.

Run the corresponding real-payload benchmarks with:

```sh
GOEXPERIMENT=simd /path/to/gotip/bin/go test -run '^$' -bench HighLevelCorpus -benchmem
```

Owned-output rows compare `encoding/json.Marshal` with `slopjson.Marshal`.
The separately labeled `slopjson-compiled-reuse` rows measure the explicit
compile-once, caller-owned-buffer API and are not presented as equivalent
allocation contracts.
