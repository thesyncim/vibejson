# Upstream

- Repository: https://go.googlesource.com/go
- Commit: `03845e30f7b73d1703bd8c21017297f6eecb76d6`
- Source: `src/encoding/json/internal/jsontest/_embed/*.json.zst`
- Models: `src/encoding/json/internal/jsontest/testdata.go`

The seven payloads are copied byte-for-byte and the concrete models are
mechanically extracted from the same revision. Run
`scripts/check-stdlib-corpus.sh` to detect additions, removals, or content
drift in the pinned Go tree.
