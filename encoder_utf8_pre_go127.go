//go:build !go1.27

package simdjson

// Go 1.26's encoding/json v1 writes the replacement rune as an escape.
const escapeInvalidUTF8 = true
