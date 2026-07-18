//go:build go1.27

package simdjson

func acceptStringTaggedNumber(string) bool {
	return true
}
