package vibejson

// decodedSimpleEscapes maps the one-byte JSON escapes to their decoded form.
// A zero entry marks an invalid escape; JSON has no simple NUL escape.
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
