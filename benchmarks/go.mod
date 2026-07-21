module github.com/thesyncim/simdjson/benchmarks

go 1.27

require (
	github.com/bytedance/sonic v1.15.2
	github.com/goccy/go-json v0.10.6
	github.com/json-iterator/go v1.1.12
	github.com/mailru/easyjson v0.9.2
	github.com/minio/simdjson-go v0.4.5
	github.com/segmentio/encoding v0.5.4
	github.com/thesyncim/simdjson v0.0.0
	github.com/thesyncim/simdjson/tests/stdlib v0.0.0
	github.com/tidwall/gjson v1.19.0
	github.com/valyala/fastjson v1.6.10
)

require (
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic/loader v0.5.1 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/modern-go/concurrent v0.0.0-20180228061459-e0a39a4cb421 // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	golang.org/x/sys v0.22.0 // indirect
)

replace github.com/thesyncim/simdjson => ..

replace github.com/thesyncim/simdjson/tests/stdlib => ../tests/stdlib
