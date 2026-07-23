# Correctness checks for the index and document-store layer

This document records two correctness checks for the navigation substrate: the
classic tape (`index.go`), the shape-deduplicated tapes (`docset_shape.go`), and
the containment evaluator (`contains.go`). Both are strong evidence, and neither
is a proof of whole-system correctness:

- **Exhaustive differential testing over a bounded domain.** Every well-formed
  JSON document up to a small depth and node bound is enumerated and checked
  equivalent to the classic reference. This is exhaustive over that bounded
  domain, not a guarantee for all inputs; raising the bound widens the domain
  the checks cover.
- **z3-checked encoding invariants.** The bit-packing arithmetic (pack/unpack
  round-trip, no field overlap) is machine-checked by z3 over the entire 32-bit
  word domain. That check is a proof of the specific algebraic invariant it
  discharges — the bit arithmetic — not a statement about the wider system.

They complement the differential and committed oracle suites. The goal is
correctness evidence, not performance.

The mutable layer adds a separate stable-slot differential in
`store_index_exact_test.go`: across chunk sizes 1, 3, 8, and 64 it interleaves
inserts, replacements, deletes, slot reuse, partial backfill, complete
backfill, and retained old snapshots, then compares every compound-index probe
against an independent full `Snapshot.Range` plus compiled-pointer walk. Nested
object fields, escaped pointer tokens, array positions, missing values, exact
decimal aliases, decoded-string aliases, deliberately coarse wide-number
buckets, and bitmap promotion/demotion are covered directly. Query-level tests
compare `RunSnapshot` against `Run` for compound equality, nested equality,
`AND`, `OR`, `NOT`, residual ranges, projections, grouping, aggregation, and
ordering in both `Building` and `Ready` states.

The two harnesses are `verify_exhaustive_test.go` and
`verify_invariants_test.go`; both run under
`go test -run 'Exhaustive|InfoWord|NarrowSpan|EncodingInvariants'`.

## Exhaustive differential testing

`verify_exhaustive_test.go` enumerates every well-formed JSON document up to a
small bound and checks the novel representations observably identical to the
classic reference on each one — exhaustive over the bounded domain rather than a
random walk through it. The enumerated domain size, which each test logs, is the
strength of the evidence: raising a bound constant widens the domain and rechecks
everything over it.

### The generator

The enumerator emits both the exact JSON bytes and an independent abstract
syntax tree for every document. The AST is the reference: it is built by the
enumerator and never by the library, so a parsed index checked against it is a
genuine cross-check. The terminal alphabet is fixed and tiny — numbers
`{0, -1, 1, 10, 1.5, 1e2}`, strings `{"", "a", "ab", "a\n", "😀"}`
(the last two carrying escapes), literals `{true, false, null}`, the two empty
containers, and object keys drawn from `{"a", "b", ""}` with duplicates arising
from repeated key sequences. Objects enumerate every ordered key sequence,
including duplicates; arrays enumerate every ordered element sequence.

Three raisable constants bound the enumeration: `bexMainDepth` (nesting),
`bexMainNodes` (total value nodes), and `bexMainWidth` (direct members per
container). At the default main bound the width cap does not bind — the node
bound is the sole limit — so the default is the genuine, unrestricted space of
documents with depth ≤ 3 and at most 4 value nodes. The width cap exists only to
keep a raised node bound from growing faster than intended; the alphabet has 14
scalar terminals, so an unrestricted flat container of width *w* alone
contributes 14^*w* documents. Representative domain sizes:

| depth | nodes | width | documents |
| ---: | ---: | ---: | ---: |
| 3 | 4 | 3 | 148,304 (default main) |
| 3 | 5 | 2 | 893,776 |
| 2 | 5 | 3 | 117,328 |
| 3 | 6 | 2 | 4,170,576 |
| 3 | 7 | 2 | 69,706,576 |

The default enumerates every dedup-eligible flat object over the key alphabet:
a shape is deduplicated only when its decoded keys are distinct, and with three
distinct keys the widest such object has three members, reached at four nodes.
Raising the node bound therefore adds classic-stored documents and wider or
deeper arrays, not new shape-tape shapes.

### Properties checked for every document

The reference is a fresh, exactly sized classic `BuildIndexOptions`, built with
the same options as the set under test. For each document and each `HashKeys`
setting:

1. **Narrow shape-tape versus classic.** A `DocSet{ShapeTapes: true}` stores
   the document (appended twice so a conforming one deduplicates on its second
   sighting). Every stored copy's `Doc(i)` widens to entries `reflect.DeepEqual`
   to the classic tape over identical source bytes, so the widening synthesis is
   byte-identical. Because a byte-identical tape makes every accessor identical,
   the accessor battery below is run against the classic reference and the AST.
2. **Wide shape-tape versus classic.** The same document is stored through the
   `wideValueTapes` test seam, which forces 16-byte value entries on
   narrow-eligible documents, and must widen to the same classic tape. The seam
   is the only way to hold both entry widths against identical documents; a
   document large enough to require the wide width naturally (a root past
   64 KiB) is impractical to enumerate, so wide-width equivalence at scale
   remains covered by the existing differential suite.
3. **Containment.** Checked pairwise in `TestExhaustiveContainsVsReference`; see
   below.
4. **Pointer/navigation.** Every reachable JSON Pointer — enumerated by an
   independent recursive descent over the AST, using last-duplicate-wins for
   object keys and positional indexing for arrays — resolves through
   `Node.Pointer` and `PointerCompiled` to the AST-specified node, and, for a
   narrow shape-taped object, through the batch `AppendPointer` path that reads
   the value slab directly rather than through widening.

The accessor battery covers `Kind`, `Raw`, `StringBytes`/`AppendText`,
`Int64`/`Uint64`/`Float64` (with the integer/float classification reproduced
from `strconv`), `Bool`, `ArrayLen`/`ObjectLen`, `Get` for every distinct key
and an absent key, `Index` in range and out of range, and `ArrayIter`/
`ObjectIter` order including duplicates. Cross-kind accessors are checked to
reject.

### Containment against an independent reference

`TestExhaustiveContainsVsReference` checks, for every ordered pair of documents
in a smaller bound, that `Node.Contains` and `RawContains` agree with
`exhaustiveContains` — a from-scratch inductive definition of PostgreSQL's jsonb
`@>` operator written directly over the AST: object containment member-wise with
last-duplicate-wins, array containment as order-insensitive duplicate-collapsing
existence, scalar equality by decoded string content and by exact rational value
for numbers (`math/big`), and the one documented top-level special case where an
array contains a scalar equal to some element. This is a differential check of
the evaluator against a separate reference, not against itself. The pair bound
(`bexPairDepth = 3`, `bexPairNodes = 3`, `bexPairWidth = 2`) includes one level
of nesting so object-of-object and array-of-array containment are exercised; the
pair count is the square of the document count, so the bound is kept small
deliberately. A disagreement on any pair is a real bug and is reported with the
minimal counterexample.

### Domain sizes at the default bound

| Harness | Domain |
| --- | --- |
| Equivalence (properties 1, 2, 4) | 148,304 documents × 2 option sets; 26,160 narrow-taped, 26,160 wide-taped, 122,144 classic-stored; 478,864 reachable pointers per option |
| Containment (property 3) | 2,896 documents; 8,386,816 ordered pairs |

`-short` (which the race and checkptr instrumentation runs pass) covers a
representative smaller bound so those runs stay fast; the full domain is the
default. Both harnesses complete in a few seconds each on a stable go1.26 build.

### Raising the bounds

Raise `bexMainDepth`, `bexMainNodes`, or `bexMainWidth` (or the `bexPair*`
constants) and rerun. `bexDomainCeiling` fails the enumeration rather than
running for hours if a bound is raised without accounting for the blowup; raise
it too when the larger domain is intended. A raised bound is strictly stronger
evidence.

## z3-checked encoding invariants

`verify_invariants_test.go` checks the bit-packing cores lossless and their
fields non-overlapping. Both cores fold several values into one 32-bit word, so
every accessor and every widening depends on the packing being a bijection on
its documented domain. The Go checks are exhaustive on the small axes and
saturating on the wide ones; the full joint domains are then machine-checked by
z3, whose `unsat` result is a proof of the specific bit-arithmetic invariant
across all 2^32 inputs — nothing broader.

### Info word

`packInfo` composes an entry's count (26 bits), kind (3 bits), and flags
(3 bits) into one word, and `Count`/`Kind`/`flags` recover them.

- `TestInfoWordFieldsDisjoint` checks the three field masks are pairwise
  disjoint and cover all 32 bits, and that the layout constants place the kind
  and flags fields exactly above the count field.
- `TestInfoWordRoundTrip` checks `packInfo` and the accessors mutually inverse.
  The kind and flags axes are enumerated exhaustively (all 64 combinations); the
  26-bit count axis is swept in full (`[0, 67108863]`) for the worst-case
  saturated kind and flags, and sampled at every boundary and across a
  randomized saturation for every kind/flags combination. `setCount` and
  `bumpCount` are checked to move only the count field.

### Narrow span

A narrow shape-tape value packs a member's source span as `start | end<<16`
under the precondition that both offsets fit 16 bits (`shapeNarrowMaxEnd`).
`TestNarrowSpanRoundTrip` sweeps each 16-bit half in full against boundary
values of the other and saturates the joint space randomly, checking `start()`,
`end()`, and `widen()` recover the exact wide entry the narrow value was packed
from and that the two halves never overlap.

### Full-word-domain proof in SMT

[`testdata/smt`](../../testdata/smt/) holds one QF_BV script per invariant, each
asserting the negation over the entire 32-bit domain and discharged by a solver
returning `unsat`. For these bit-arithmetic invariants that `unsat` is a proof
over the whole word domain:

| Script | Machine-checks |
| --- | --- |
| `info_word.smt2` | `packInfo`/accessor round-trip for every in-range triple |
| `info_word_disjoint.smt2` | the count/kind/flags masks partition the word |
| `narrow_span.smt2` | the span round-trips and its halves are disjoint |

`TestEncodingInvariantsSMT` checks each committed script still models the code's
live constants — a layout change that outdates a script fails the test rather
than passing silently — and, when `z3` is on `PATH`, runs it and requires
`unsat`. When no solver is present the scripts stand as the committed proof
obligation for the bit arithmetic, and the Go checks cover the same invariants
over the bounded domain. `scripts/verify-smt.sh` discharges the scripts with any
conforming solver (`Z3=/path/to/solver` overrides the default) and records the
verbatim result to `testdata/smt/z3-results.log`, the artifact that pins the
result. The scripts model fixed machine-word domains and never need widening as
the Go bounds rise; they change only when a layout constant does.

## What is checked, and how strongly

| Invariant | Go (bounded, exhaustive/saturating) | z3 (full word domain) |
| --- | --- | --- |
| Shape-tape ⇔ classic (narrow, wide) | every document to the default bound | — |
| Accessors ⇔ AST | every document to the default bound | — |
| Pointer navigation ⇔ recursive descent | every reachable pointer to the bound | — |
| `Contains`/`RawContains` ⇔ `@>` reference | every ordered pair to the pair bound | — |
| Info-word round-trip and field partition | kind/flags exhaustive, count swept + saturated | machine-checked over 2^32 |
| Narrow-span round-trip and half-disjointness | each 16-bit half swept, joint saturated | machine-checked over 2^32 |

The shape-tape, navigation, and containment checks range over document
structure, whose space is infinite, so they are exhaustive over a raisable
finite slice rather than a proof for all inputs. The two bit-packing invariants
range over fixed machine words, so z3 machine-checks them across the whole
domain.
