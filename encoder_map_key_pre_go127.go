//go:build !go1.27

package simdjson

// encoding/json v1 gives string kinds precedence over TextMarshaler.
const mapKeyStringKindFirst = true
