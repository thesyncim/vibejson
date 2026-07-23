module github.com/thesyncim/slopjson/tests/stdlib

go 1.26

require (
	github.com/klauspost/compress v1.19.0
	github.com/thesyncim/slopjson v0.0.0
)

require golang.org/x/sys v0.47.0 // indirect

replace github.com/thesyncim/slopjson => ../..
