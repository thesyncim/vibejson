# Toolchain policy

The root module requires Go 1.26. Stable compilers build the tuned portable
implementation; the SIMD implementation uses the experimental `simd/archsimd`
API and is isolated to the Go 1.27 compiler family. Go release tags are
cumulative, so accelerated files use `go1.27 && !go1.28`; their paired
fallbacks include the exact complementary `!go1.27 || go1.28` constraint.
Later compilers therefore remain portable until they are validated and
promoted deliberately.

## Supported compiler

The latest Go 1.26 patch release is the supported stable toolchain. For SIMD,
the authoritative compiler revision is `go_tip_commit` in
[`scripts/bootstrap-gotip.sh`](../scripts/bootstrap-gotip.sh). CI and published
SIMD benchmarks build that exact revision. A newer development revision may
work, but is best effort until it passes the same test and benchmark gates and
the pin is advanced.

The default build omits `GOEXPERIMENT=simd` and uses portable Go kernels. With
Go 1.26, the portable source set is retained even if that experiment name is
set. The pinned compiler selects accelerated kernels when `GOEXPERIMENT=simd`
is set; those kernels are maintained for amd64 and arm64. On amd64,
`GOAMD64=v1` and `v2` keep the Stage 1 classifier portable because its SIMD
implementation lowers to AVX instructions. Their scanner uses one startup CPU
capability result through an inlined direct-call branch. `GOAMD64=v3` and newer
binaries compile both Stage 1 and scanner calls directly to the AVX path, which
those architecture levels require. Dense bitmap kernels use 256-bit AVX2:
v1/v2 perform one process-constant runtime capability branch per bitmap call
and fall back to scalar, while v3+ calls the AVX2 body directly. Buffers below
eight words use the unrolled scalar loop at every amd64 level because the
two-vector AVX2 body cannot run. CI executes the
default amd64 binary and a native v3 binary, rejects AVX instructions in the
v1/v2 Stage 1 kernel package, and disassembles all bitmap levels to prove the
v1/v2 guard and retained AVX2 instructions. CI also
simulates the next compiler release tag on native amd64 and arm64 to prove that
`GOEXPERIMENT=simd` still selects a complete portable source set,
and cross-compiles portable 386 and s390x builds to cover 32-bit and
big-endian assumptions.

| Configuration | Support |
| --- | --- |
| Latest Go 1.26 patch release, portable build | Required |
| Go 1.26 with `GOEXPERIMENT=simd` | Supported portable fallback |
| Pinned Go 1.27 development compiler, portable build | Required |
| Pinned Go 1.27 development compiler, `GOEXPERIMENT=simd`, amd64 or arm64 | Required |
| Go 1.28 or later, default build | Portable until a release-specific SIMD promotion passes all gates |
| Newer Go 1.27 development revision | Best effort until pinned |
| Stable Go release before 1.26 | Unsupported |

## Advancing the pin

An update to the compiler revision is a compatibility change. The same commit
must:

1. update `go_tip_commit` and any required bootstrap version;
2. regenerate checked-in output and tidy module files;
3. pass generic and SIMD tests, race detection, checkptr, vet, static analysis,
   fuzz smoke, corpus parity, and cross-compilation;
4. compare the maintained high-level benchmarks against the previous compiler
   revision on the dedicated runner; and
5. record any compiler or `archsimd` behavior change that required source
   changes.

A release is cut only from a revision green under the pinned compiler. Stable
SIMD support for a new compiler family is declared only after correctness,
escape analysis, disassembly, allocation, and native per-architecture
performance gates pass. If its API and optimal source are unchanged, extend
the validated release window; fork only kernels that actually need different
source. Support is never inferred from an unpinned build that merely compiles.

## Static-analysis exceptions

CI runs the pinned staticcheck release against production and test code with all
`SA` checks enabled. Intentional exceptions are local `//lint:ignore` directives
instead of workflow-wide exclusions:

- `SA5008` is suppressed on malformed struct tags that are explicit
  `encoding/json` compatibility inputs.

The source line next to every exception states the reason. The malformed-tag
exceptions remain only while those exact compatibility cases remain tests.
