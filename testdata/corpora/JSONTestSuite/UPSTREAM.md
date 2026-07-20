# JSONTestSuite provenance

- Provenance: JSONTESTSUITE-001
- Repository: https://github.com/nst/JSONTestSuite
- Commit: `1ef36fa01286573e846ac449e8683f8833c5b26a`
- Imported: 2026-07-10
- Upstream path: `test_parsing/`
- Local changes: none
- License: MIT; see `LICENSE`

The filename prefix is the expected parse result defined by upstream:

- `y_`: must be accepted
- `n_`: must be rejected
- `i_`: implementation-defined

simdjson applies an explicit strict policy to the `i_` group. Arbitrarily large
JSON number spellings and 500 nested arrays are accepted. UTF-8 BOMs, UTF-16,
invalid UTF-8, non-Unicode code points, and unpaired or inverted UTF-16
surrogates are rejected.
