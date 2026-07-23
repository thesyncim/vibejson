module github.com/thesyncim/simdjson/tests/stdlib

go 1.26

require (
	github.com/klauspost/compress v1.19.0
	github.com/thesyncim/simdjson v0.0.0
)

require golang.org/x/sys v0.47.0 // indirect

replace github.com/thesyncim/simdjson => ../..
