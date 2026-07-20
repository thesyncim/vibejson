package scanner

// Info describes the scanner implementation selected during initialization.
type Info struct {
	Enabled     bool
	Backend     string
	VectorBytes int
	MinBytes    int
}

// Current reports the selected scanner backend.
func Current() Info {
	return currentInfo()
}
