//go:build !go1.27

package slopjson

func acceptStringTaggedNumber(text string) bool {
	// encoding/json v1 rejects quoted numeric literals before strconv unless
	// their first byte could begin a JSON number. The rest is still parsed by
	// strconv, preserving legacy forms such as leading zeroes and hex floats.
	return len(text) != 0 && (text[0] == '-' || text[0] >= '0' && text[0] <= '9')
}
