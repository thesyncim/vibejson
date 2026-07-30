# Documentation map

The documentation is organized by audience:

| Document | Audience and authority |
| --- | --- |
| [README](../README.md) | Users choosing and integrating an API |
| [Package documentation](../doc.go) | Exact exported behavior and lifetime contracts |
| [Architecture](architecture.md) | Maintainers reviewing package boundaries and execution design |
| [Benchmarking](benchmarking.md) | Contributors running coverage suites or regression gates |
| [Benchmark snapshot](../benchmarks/README.md) | Users reviewing absolute comparison results and their fairness contract |
| [Contributing](../CONTRIBUTING.md) | Required development and review workflow |
| [Migration](../MIGRATION.md) | Users moving from the former module or database layout |
| [Security](../SECURITY.md) | Supported revisions, private reporting, and deployment controls |
| [Provenance](provenance.md) | External source, algorithm, corpus, and license ledger |
| [Unsafe inventory](../UNSAFE.md) | Generated unsafe scopes, invariants, tests, and benchmarks |

`maintenance-baseline.json` is an intentionally immutable historical
measurement captured at the commit recorded inside the file. It is not a
description of the current tree. Current source counts, API shape, fuzz targets,
and benchmark results must be measured from the checked-out revision.
