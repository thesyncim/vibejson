package vibejson

// decodedSimpleEscapes maps the one-byte JSON escapes to their decoded form.
// A zero value marks an invalid simple escape; JSON has no simple escape for
// NUL, so every valid mapping remains distinguishable.
var decodedSimpleEscapes = [256]byte{
	'"':  '"',
	'\\': '\\',
	'/':  '/',
	'b':  '\b',
	'f':  '\f',
	'n':  '\n',
	'r':  '\r',
	't':  '\t',
}
