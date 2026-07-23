module github.com/thesyncim/slopjson/benchmarks

go 1.27

require (
	github.com/thesyncim/slopjson v0.0.0
	github.com/thesyncim/slopjson/tests/stdlib v0.0.0
)

require (
	github.com/klauspost/compress v1.19.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/thesyncim/slopjson => ..

replace github.com/thesyncim/slopjson/tests/stdlib => ../tests/stdlib
