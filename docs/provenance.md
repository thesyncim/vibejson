# Provenance

This is the canonical inventory for externally derived source, algorithms,
generated output, tests, and corpora. A compact `Provenance: ID` comment at an
implementation site resolves to one row here. Conceptual similarity is not
listed as copied code, and uncertain origins stay explicitly unresolved.

This repository is an independent Go implementation and is not affiliated with
the C++ [`simdjson`](https://github.com/simdjson/simdjson) project.

## Release status

The repository has no root project license. `LICENSE-GO` contains the Go
Authors' BSD-3-Clause text for identified Go-derived material.
`LICENSE-SIMDJSON` contains the Apache-2.0 text selected for identified material
adapted from the dual-licensed C++ simdjson project. Neither file licenses the
repository as a whole. A root `LICENSE` and final `NOTICE` remain release
requirements.

## Source and algorithm ledger

| ID | Local material | Authoritative source and license | Local changes and integrity |
| --- | --- | --- | --- |
| `GO-EISEL-001` | `internal/floatconv/eisel.go:EiselLemire64`; generated `internal/floatconv/eisel_table.go` | Go 1.25.0, commit `6e676ab2b809d46623acb5988248d95d1eb7939c`, `src/strconv/eisel_lemire.go`; Go Authors, BSD-3-Clause, `LICENSE-GO` | Float64-only JSON integration and expanded comments. The `math/big` generator is local; all 696 output rows are mathematically equivalent to Go's table. `go generate` is the integrity check. Upstream records deeper lineage to `fast_double_parser` `644bef4306059d3be01a04e77d3cc84b379c596f`, Wuffs `ba3818cb6b473a2ed0b38ecfc07dbbd3a97e8ae7`, and Nigel Tao's test data `5280dcfccf6d0b02a65ae282dad0b6d9de50e039`. |
| `GO-NUMSCAN-001` | `number_float.go:{jsonNumber,scanJSONNumber,addDigit,add16Digits,exactFloat64}` | Go commit `d468ad3648be469ffc4090e4586c29709182d6b6`, `src/internal/strconv/atof.go:{readFloat,atof64exact}`; Go Authors, BSD-3-Clause, `LICENSE-GO` | Strict JSON grammar, unsafe byte-span input, 16-digit batches, and local exact-scale/Eisel fallbacks. Differential float tests are the parity proof. |
| `GO-FIELDS-001` | `internal/jsonfields/jsonfields.go:{Resolve,validTag}` | Go commit `d468ad3648be469ffc4090e4586c29709182d6b6`, `src/encoding/json/encode.go:{typeFields,isValidTag}`; Go Authors, BSD-3-Clause, `LICENSE-GO` | Compiled-field layout, pointer-hop metadata, and opt-in `,inline` support. Struct-field parity tests cover visibility and dominance. |
| `GO-CYCLE-001` | `encoder_cycle_pre_go127.go:enterReference/leaveReference` | Go 1.26.4, commit `a9ce111d580581fb925ae88f125c69b7d93504ea`, `src/encoding/json/encode.go`; Go Authors, BSD-3-Clause, `LICENSE-GO` | Unified local key and `EncodeError`; the delayed threshold remains 1,000. Cycle and allocation tests cover the behavior. |
| `GO-STRING-001` | `encoder_string.go:appendEncodedJSONString`; `encode.go:appendJSONStringBytes`; `marshaler.go:escapeHTMLInPlaceTail` | Conservatively treated as adaptations of Go commit `d468ad3648be469ffc4090e4586c29709182d6b6`, `src/encoding/json/encode.go:appendString`, and pinned Go commit `03845e30f7b73d1703bd8c21017297f6eecb76d6`, `src/encoding/json/indent.go:appendHTMLEscape`; Go Authors, BSD-3-Clause, `LICENSE-GO` | SIMD prefix scanning, fused copying, byte-slice/in-place integration, and compiler-version invalid-UTF-8 spelling are local. Encoding parity tests cover the result. |
| `GO-USCALE-001` | `number_uscale.go` | Go commit `d468ad3648be469ffc4090e4586c29709182d6b6`, `src/internal/strconv/uscale.go`; Go Authors, BSD-3-Clause, `LICENSE-GO` | Parsing half specialized to JSON's negative decimal exponents. Differential float tests cover exact bits. |
| `GO-FLOATFMT-001` | `simd/float.go`, `simd/float_pow10.go` | Go commit `03845e30f7b73d1703bd8c21017297f6eecb76d6`, `src/internal/strconv/{uscale.go,ftoa.go,pow10tab.go}`; Go Authors, BSD-3-Clause, `LICENSE-GO` | JSON spelling and caller-owned output specialization. Differential formatting tests and generated checks cover parity. |
| `GO-DATE-001` | `simd/time_date.go` | Go commit `03845e30f7b73d1703bd8c21017297f6eecb76d6`, `src/time/time.go:absDays.date` and helpers; Go Authors, BSD-3-Clause, `LICENSE-GO`. Algorithm: Cassio Neri and Lorenz Schneider, “Euclidean affine functions and their application to calendar algorithms,” 2023, DOI `10.1002/spe.3172` | Direct JSON time-format integration. Exhaustive date tests cover the supported range. |
| `CPP-UTF8-001` | `internal/scanner/scan_simd_arm64.go` UTF-8 lookup tables and kernels | C++ simdjson 4.6.4, commit `1bcf71bd85059ab6574ea1159de9298dcc1212c5`, `src/generic/stage1/utf8_lookup4_algorithm.h`; Apache-2.0, `LICENSE-SIMDJSON`. Algorithm: Keiser and Lemire, “Validating UTF-8 In Less Than One Instruction Per Byte,” 2020 | Go SIMD translation, scalar tails, and fused U+2028/U+2029 detection. SIMD/scalar parity and UTF-8 corpus tests cover the result. |
| `CPP-STAGE1-001` | Production Stage 1 family under `internal/kernels/` | Verified C++ simdjson 4.6.4 reference commit `1bcf71bd85059ab6574ea1159de9298dcc1212c5`; Apache-2.0, `LICENSE-SIMDJSON`. Paths: `src/generic/stage1/{json_escape_scanner.h,json_string_scanner.h,json_scanner.h,json_structural_indexer.h}`, `include/simdjson/arm64/{bitmask.h,simd.h}`, and `src/arm64.cpp`. Design: Geoff Langdale and Daniel Lemire, [“Parsing Gigabytes of JSON per Second”](https://arxiv.org/abs/1902.08318), VLDB Journal 28(6), 2019 | Adapted elements are the backslash carry, prefix-XOR/string pipeline shape, ARM64 mask reduction, structural writer shape, and classifier structure. Scalar table packing, Go SIMD reductions, batching, metadata, specializations, thresholds, and fused consumers are local. The exact historical origin revision was not recorded; the named commit is a verified reference, not a guessed origin. Native scalar/SIMD parity tests are the integrity proof. |
| `CPP-WALK-001` | `index.go:walkFast` | C++ simdjson 4.6.4, commit `1bcf71bd85059ab6574ea1159de9298dcc1212c5`, `src/generic/stage2/json_iterator.h:json_iterator::walk_document`; Apache-2.0, `LICENSE-SIMDJSON` | Go-owned tape, exact error offsets, local primitive scanners, and local storage contracts. DOM/index differential tests cover behavior. |
| `ALGO-DIGITS-001` | `simd/digits.go:parse8DigitsWord`; `number_digits.go:parse8DigitsWord` | Exact three-constant reduction: Johnny Lee, [“Fast numeric string to int”](https://johnnylee-sde.github.io/Fast-numeric-string-to-int/) (2016), which credits user `bormand`; also C++ simdjson 4.6.4 commit `1bcf71bd85059ab6574ea1159de9298dcc1212c5`, `include/simdjson/arm64/numberparsing_defs.h`, which preserves Lee's explanation. Related SWAR derivation: Daniel Lemire, “Quickly parsing eight digits” (2018) | Classified as algorithm-derived because the historical local source was not recorded. Exhaustive digit tests cover the result. |

## Generated material and corpora

| ID | Local material | Source, license, and local treatment |
| --- | --- | --- |
| `GO-CORPUS-001` | `tests/stdlib/testdata`, `tests/stdlib/models.go` | Pinned Go commit `03845e30f7b73d1703bd8c21017297f6eecb76d6`, source paths recorded in `tests/stdlib/README.md`, and BSD license in adjacent `testdata/LICENSE`. Payloads are byte-for-byte copies and the canonical models are mechanically extracted once in `tests/stdlib`. The corpus material is byte-identical to the prior `d468ad3648be469ffc4090e4586c29709182d6b6` pin. `scripts/check-stdlib-corpus.sh` verifies the source revision, generated corpus, and canonical models. |
| `JSONTESTSUITE-001` | `testdata/corpora/JSONTestSuite` | `nst/JSONTestSuite` commit `1ef36fa01286573e846ac449e8683f8833c5b26a`; MIT; no local corpus changes. Pin and policy are beside the corpus in `UPSTREAM.md` and `LICENSE`. |
| `CPP-UTF8-TEST-001` | `validation_corpus_test.go` cases named by its source comment | Direct source: C++ simdjson commit `9b33047a878264250c5361f865d0b2da86217d14`, `tests/unicode_tests.cpp`; Apache-2.0 at that revision, `LICENSE-SIMDJSON`; local test conversion only. That source credits the Autobahn WebSocket TestSuite for its additional numbered sequences; its exact Autobahn revision was not recorded there and remains unresolved | Preserve the exact direct-source comment, the transitive Autobahn credit, and coverage in the Unicode corpus tests. |

Corpus-only dependencies are not copied into the root package. Their nested
`go.mod` and `go.sum` files are the authoritative version inventory.

## Unresolved origins

These items must not receive a guessed attribution:

- `encoder_int.go` uses `((bits.Len64(v)*1233)>>12)` for decimal digit count.
  History says the trick was borrowed but does not name its source.
- `number_exactness_test.go:TestFloatHardCases` combines boundary families that
  partly overlap C++ simdjson and classic strtod stress suites. The original
  change did not record an exact source for every string.
- `testdata/FUZZ_CORPUS.json` records ownership and hashes but not complete
  discovery, derivation, license, or introduction history for every seed.

Resolve an item only from documentary evidence. Until then, preserve the
warning at its implementation or inventory site.

## Work currently classified as local

The audit found no source-copy evidence for the Stage 2 pair-table DFA and
generated/goto machines; compiled typed plans and executors; hooks and stream
state machines; SIMD thresholds; most string scanning; Unicode-escape phase
tables; fused line-separator logic; ARM64 digit formatting; synthetic benchmark
models; or benchmark adapters. On-Demand-style and “analogue” comments are
conceptual acknowledgements, not source lineage.

## Maintenance rule

Before adding or changing externally related material:

1. record a stable ID, project or paper, exact revision, path or section,
   upstream license, local changes, confidence, and integrity proof here;
2. add `Provenance: ID` at each adapted implementation site;
3. keep required upstream license text with the repository or vendored corpus;
4. never replace missing history with a plausible guess; and
5. update the final `NOTICE` in the same change once that file exists.
