module github.com/thesyncim/vibejson/benchmarks

go 1.27

require (
	github.com/thesyncim/vibejson v0.0.0
	github.com/thesyncim/vibejson/tests/stdlib v0.0.0
)

require (
	github.com/klauspost/compress v1.19.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/thesyncim/vibejson => ..

replace github.com/thesyncim/vibejson/tests/stdlib => ../tests/stdlib
