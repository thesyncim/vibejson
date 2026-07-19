# Equivalent C++/Go corpus benchmark

This directory contains one direct cross-language comparison:
`parse+semantic-digest`. Representation-specific DOM, typed decode,
validation-only, and serialization benchmarks do different work and are not
ranked against it.

## Enforced contract

Every timed iteration:

1. parses the complete, already-loaded source with reusable parser storage;
2. visits every array element and object member in source order;
3. decodes every object key and string value;
4. decodes every number to the same signed, unsigned, binary64, or big-integer
   category; and
5. hashes the complete semantic event stream with the same 64-bit FNV-1a
   algorithm.

File I/O, capacity discovery, reusable storage allocation, and reference-digest
construction are outside the timer. Grammar validation, unescaping, number
conversion, complete traversal, and digest construction are inside it.

The runner compares every C++ and Go digest and fails before publication if a
pair differs. The identical digest for the escaped-string and Unicode-string
fixtures is expected: they decode to the same semantic value.

The C++ control pins simdjson 4.6.4 at commit
`1bcf71bd85059ab6574ea1159de9298dcc1212c5`. C++ and Rust dependency revisions
are pinned; changing one creates a different benchmark record. Machine,
compiler, implementation selection, samples, digests, and timings are stored in
[`../results/latest.json`](../results/latest.json).

## Reproduce

The runner requires `clang++`, `cargo`, `zstd`, git, and the repository's pinned
Go binary:

```sh
TIP_GO="$HOME/sdk/simdjson-gotip/bin/go" ./benchmarks/crosslang/run.sh
```

It refuses a dirty repository by default and prints the exact revisions and
selected native implementation.
