# Security policy

## Supported revisions

No tagged release exists. Security fixes are made on `main`; consumers must
upgrade to a fixing revision after reviewing and validating it for their
deployment.

No support window or backport policy is implied before releases begin. A
versioned support table will replace this section when tagged releases exist.

## Report a vulnerability

GitHub private vulnerability reporting is not currently enabled for this
repository. Open a [public issue requesting a private contact
channel](https://github.com/thesyncim/vibejson/issues/new), but include no
exploit details, private inputs, working reproducer, or other sensitive
technical information in the issue. The maintainer will arrange a private
channel for the report.

This section must be updated before private vulnerability reporting is enabled
or a dedicated security address is published.

Include, when available:

- the affected repository revision;
- Go version or commit, architecture, `GOEXPERIMENT`, and build tags;
- the smallest reproducer and whether it is safe to share;
- whether the issue requires portable, SIMD, streaming, zero-copy, or native
  hook paths;
- expected confidentiality, integrity, availability, or resource-exhaustion
  impact; and
- any known workaround.

## Relevant security boundaries

Reports are in scope when untrusted input can cause or expose:

- acceptance of invalid JSON or disagreement between validation and decoding;
- out-of-bounds access, checkptr failure, stale pointer use, or GC corruption;
- a borrowed value surviving beyond its documented storage lifetime;
- mutation or corruption through aliasing in reused destinations;
- incorrect stream framing, value-limit bypass, or hidden source errors;
- unbounded resource retention after a transient large input;
- custom marshal/unmarshal dispatch that violates receiver or input ownership;
  or
- a practical denial of service beyond documented depth and value-size policy.

Ordinary API misuse outside a documented precondition, benchmark variance, and
compatibility changes clearly allowed by the pre-v1 status are not normally
security vulnerabilities, but reports that reveal an unsafe or ambiguous
contract are still welcome.

## Validation process

Reports remain private while maintainers reproduce the issue, determine the
affected revisions and backends, prepare a regression test, and validate the
fix through the relevant portable, SIMD, race, checkptr, fuzz, corpus,
concurrency, and performance lanes.

No fixed response or disclosure deadline is promised. Coordinated disclosure
timing will be discussed in the private report.

## Deployment guidance

For untrusted or network input:

- use `NewReaderWithOptions` with a protocol-appropriate positive
  `MaxValueBytes`;
- set a bounded `DecoderOptions.MaxDepth` or `document.IndexOptions.MaxDepth`
  appropriate to the application;
- do not retain `RawValue`, `Index`, reader-buffer views, or zero-copy decoded
  strings beyond their documented source lifetime;
- do not mutate borrowed source bytes while results are reachable; and
- keep the Go toolchain and the library revision current.

These controls reduce exposure but do not replace reporting a suspected
vulnerability.
