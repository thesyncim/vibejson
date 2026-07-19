# Pooling and retained-resource bounds

Pools remove repeated allocation only when their cleanup and retained capacity
are bounded. An exceptional input must not set the permanent cost of later
small operations.

## Encoder scratch

Each compiled encoder owns a pool whose scratch layout is fixed by its plan.
Pointer-bearing reflect values keep their concrete type permanently; scratch is
never reinterpreted for another type. Map entries, rendered key bytes, and
typed value backing each have a 512 KiB retention budget. A map above the
lowest applicable element limit uses one-shot storage and cannot replace warm
pooled scratch.

Cleanup clears only the prefix populated during the operation. Map iterators
are reset before pooling, user values are zeroed, and numeric key strings do not
escape their byte arena. Dynamic interface plans use a separate bounded
polymorphic slot because their layout is not part of the enclosing static plan.

## Structural decoder state

Typed structural tapes retain at most 2 MiB of `uint32` positions. A larger
tape is dropped when the call releases its state. Escaped-string arenas are
detached before pooling because decoded results may still own their backing.

## Marshal output hints

The convenience `Marshal` cache retains only an integer size estimate, never
output storage. Ordinary observations through 256 KiB update the estimate
immediately, with a 64-byte minimum initial capacity. A first larger observation
is kept as an unconfirmed outlier and gives the next call a 512-byte initial
capacity. A repeated equal large observation confirms that workload and enables
exact presizing, even above 256 KiB. A smaller observation replaces either state
immediately. The 256 KiB value is therefore an outlier-confirmation threshold,
not a retained-size ceiling. Long-lived high-volume callers should reuse
`Encoder.AppendJSON` output storage instead.

## Required evidence

Resource tests inspect retained capacity and pointer clearing after
huge-then-small sequences. The performance gate includes
`BenchmarkEncodeTinyAfterHuge`, `BenchmarkStructuralTapeTinyAfterHuge`,
and `BenchmarkMarshalSmall`. Any new pool needs a documented byte or element
bound, a forced-GC retention test, a small-after-large latency benchmark, and an
error-path cleanup test.
