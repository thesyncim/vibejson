module github.com/thesyncim/vibejson/benchmarks

go 1.27

require (
	github.com/goccy/go-json v0.10.6
	github.com/json-iterator/go v1.1.12
	github.com/segmentio/encoding v0.5.4
	github.com/thesyncim/vibejson v0.0.0
	github.com/thesyncim/vibejson/tests/stdlib v0.0.0
)

require (
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180228061459-e0a39a4cb421 // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	golang.org/x/sys v0.0.0-20211110154304-99a53858aa08 // indirect
)

replace github.com/thesyncim/vibejson => ..

replace github.com/thesyncim/vibejson/tests/stdlib => ../tests/stdlib
