//go:build !go1.27

package vibejson

// Go through 1.26 commits no terminal scalar without a following delimiter
// when a non-EOF source error arrives with its final byte.
func terminalValueNeedsSourceBoundary(leading byte) bool {
	return leading != '{' && leading != '['
}
