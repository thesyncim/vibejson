package easyjsonmodel

// Provenance: GEN-EASYJSON-001. The generated output, exact generator version,
// command, and MIT license are recorded in ../../docs/provenance.md.
//go:generate go run github.com/mailru/easyjson/easyjson -all model.go

type TypedSmall struct {
	ID   int    `json:"id"`
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

type TypedRecord struct {
	ID      int        `json:"id"`
	Active  bool       `json:"active"`
	Name    string     `json:"name"`
	Message string     `json:"message"`
	Scores  [3]float64 `json:"scores"`
}

type TypedMeta struct {
	Count  int    `json:"count"`
	Source string `json:"source"`
}

type TypedDocument struct {
	Items []TypedRecord `json:"items"`
	Meta  TypedMeta     `json:"meta"`
}
