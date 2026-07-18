//go:build go1.27

package simdjson

// Go 1.27's encoding/json writes the replacement rune literally.
const escapeInvalidUTF8 = false
