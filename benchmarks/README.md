# slopjson benchmarks

This nested module contains standalone product benchmarks. It has no
cross-library scoreboard or external-engine harness.

The benchmark families cover:

- strict validation on deterministic small, medium, and large fixtures;
- dynamic and typed decode with owned, source-backed, and reused destinations;
- reusable structural indexing;
- full semantic traversal;
- compiled and one-shot encoding;
- production stage-1 and stage-2 kernels; and
- the pinned real-world corpus shared with `tests/stdlib`.

Fixture preparation, compressed-corpus reads, model selection, decoder and
encoder compilation, capacity discovery, and correctness checks remain outside
timed regions. Timed rows report throughput, `B/op`, and `allocs/op`.

Run the native suite with the pinned compiler:

```sh
cd benchmarks
"$HOME/sdk/slopjson-gotip/bin/go" test ./...
"$HOME/sdk/slopjson-gotip/bin/go" test -run '^$' -bench . -benchmem
```

Run one real-corpus family in both supported compiler modes:

```sh
cd benchmarks
GOEXPERIMENT=nosimd "$HOME/sdk/slopjson-gotip/bin/go" test \
  -run '^$' -bench '^BenchmarkCorpus$' -benchmem -count 6
GOEXPERIMENT=simd "$HOME/sdk/slopjson-gotip/bin/go" test \
  -run '^$' -bench '^BenchmarkCorpus$' -benchmem -count 6
```

## Regression gate

Performance changes are judged against the preceding green revision with the
interleaved root benchmark gate:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT= \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63
BENCH_GO="$HOME/sdk/slopjson-gotip/bin/go" BENCH_GOEXPERIMENT=simd \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63
```

The gate rejects statistically significant `sec/op` regressions above 2% and
any significant `B/op` or `allocs/op` increase. A targeted gate must specify
the exact benchmark selector and row count. Hosted architecture jobs provide
directional product-backend evidence; the dedicated ARM64 lane owns hard
performance decisions.
