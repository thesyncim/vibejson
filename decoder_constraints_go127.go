//go:build go1.27

package vibejson

// stringValue is the set of string types accepted by decoderCursor.String.
type stringValue interface {
	~string
}

// boolValue is the set of boolean types accepted by decoderCursor.Bool.
type boolValue interface {
	~bool
}
