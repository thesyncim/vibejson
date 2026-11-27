package e2e

//go:generate go run ../.. -type=Order,Address

type Address struct {
	Street string `json:"street"`
	City   string `json:"city"`
	Zip    int    `json:"zip,omitempty"`
}

type Order struct {
	ID       int64      `json:"id"`
	Active   bool       `json:"active"`
	Customer string     `json:"customer"`
	Note     string     `json:"note,omitempty"`
	Scores   [3]float64 `json:"scores"`
	Tags     []string   `json:"tags,omitempty"`
	Shipping *Address   `json:"shipping,omitempty"`
	Billing  Address    `json:"billing"`
	Ignored  string     `json:"-"`
	private  int
}
