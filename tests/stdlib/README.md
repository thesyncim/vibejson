# Go standard library corpus

This nested test module runs simdjson against every high-level JSON payload in
the pinned Go revision's `encoding/json/internal/jsontest` corpus. Keeping it
in a nested module leaves simdjson's root `go.mod` dependency-free;
`klauspost/compress` is used only here to read the standard library's checked-in
Zstandard assets.

The payloads are copied byte-for-byte from the exact Go revision pinned by
`../../scripts/bootstrap-gotip.sh`. Refresh and verify them with:

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

Owned-output rows compare `encoding/json.Marshal` with `simdjson.Marshal`.
The separately labeled `simdjson-compiled-reuse` rows measure the explicit
compile-once, caller-owned-buffer API and are not presented as equivalent
allocation contracts.

The corpus files and derived metadata are covered by Go's BSD license; see
`testdata/LICENSE` and `testdata/UPSTREAM.md`.
