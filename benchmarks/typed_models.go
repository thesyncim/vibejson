package benchmarks

// TypedSmall is the compact typed-codec benchmark fixture.
type TypedSmall struct {
	ID   int    `json:"id"`
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

// TypedRecord is one representative record in the medium and large typed
// benchmark documents.
type TypedRecord struct {
	ID      int        `json:"id"`
	Active  bool       `json:"active"`
	Name    string     `json:"name"`
	Message string     `json:"message"`
	Scores  [3]float64 `json:"scores"`
}

// TypedMeta is the metadata section of a TypedDocument fixture.
type TypedMeta struct {
	Count  int    `json:"count"`
	Source string `json:"source"`
}

// TypedDocument is the reusable medium and large typed benchmark fixture.
type TypedDocument struct {
	Items []TypedRecord `json:"items"`
	Meta  TypedMeta     `json:"meta"`
}
