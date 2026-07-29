//go:build go1.27

package vibejson

// Go 1.27 commits complete strings and fixed-length literals without a
// following delimiter. Numbers can still continue and retain the source-error
// precedence used by earlier toolchains.
func terminalValueNeedsSourceBoundary(leading byte) bool {
	switch leading {
	case '{', '[', '"', 'n', 't', 'f':
		return false
	default:
		return true
	}
}
