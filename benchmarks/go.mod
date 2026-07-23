module github.com/thesyncim/simdjson/benchmarks

go 1.27

require (
	github.com/thesyncim/simdjson v0.0.0
	github.com/thesyncim/simdjson/tests/stdlib v0.0.0
)

require (
	github.com/klauspost/compress v1.19.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/thesyncim/simdjson => ..

replace github.com/thesyncim/simdjson/tests/stdlib => ../tests/stdlib
