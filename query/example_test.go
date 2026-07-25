package query_test

import (
	"fmt"
	"log"

	"github.com/thesyncim/vibejson/query"
	"github.com/thesyncim/vibejson/store"
)

func exampleDocSet() *store.DocSet {
	set := &store.DocSet{}
	for _, doc := range []string{
		`{"team":"infra","tier":"pro","score":7,"active":true}`,
		`{"team":"infra","tier":"free","score":3,"active":true}`,
		`{"team":"data","tier":"pro","score":9,"active":true}`,
		`{"team":"data","tier":"team","score":4,"active":false}`,
		`{"team":"web","tier":"free","score":5,"active":true}`,
	} {
		if _, err := set.Append([]byte(doc)); err != nil {
			log.Fatal(err)
		}
	}
	return set
}

// A query written as a JSON document. Sibling keys of a filter conjoin, so an
// all-of condition needs no explicit operator.
func ExampleNew() {
	q, err := query.New(query.M{
		"select": query.A{
			"team",
			query.M{"total": query.M{"$sum": "score"}},
			query.M{"n": query.M{"$count": nil}},
		},
		"where": query.M{
			"active": true,
			"tier":   query.M{"$in": query.A{"pro", "team"}},
		},
		"groupBy": "team",
		"orderBy": query.A{query.M{"team": "asc"}},
	})
	if err != nil {
		log.Fatal(err)
	}

	result, err := q.Run(exampleDocSet())
	if err != nil {
		log.Fatal(err)
	}
	teams, _ := result.Column("team")
	totals, _ := result.Column("total")
	counts, _ := result.Column("n")
	for row := range result.RowCount {
		fmt.Printf("%s total=%s n=%s\n", teams.Cells[row], totals.Cells[row], counts.Cells[row])
	}
	// Output:
	// "data" total=9 n=1
	// "infra" total=7 n=1
}

// The same query as JSON text. A query can therefore be stored in a config
// file or received over a wire and compiled to the same plan; every number
// keeps its original spelling, so an integer past float64's exact range
// compiles to an exact literal.
func ExampleParse() {
	q, err := query.Parse([]byte(`{
		"select":  ["team", {"best": {"$max": "score"}}],
		"where":   {"score": {"$gte": 4}},
		"groupBy": ["team"],
		"orderBy": [{"team": "asc"}]
	}`))
	if err != nil {
		log.Fatal(err)
	}

	result, err := q.Run(exampleDocSet())
	if err != nil {
		log.Fatal(err)
	}
	teams, _ := result.Column("team")
	best, _ := result.Column("best")
	for row := range result.RowCount {
		fmt.Printf("%s best=%s\n", teams.Cells[row], best.Cells[row])
	}
	// Output:
	// "data" best=9
	// "infra" best=7
	// "web" best=5
}

// In is the membership form of an equality. It compiles to a sorted set that
// the executor binary-searches once per row, so its cost grows with the log of
// the alternatives rather than with their number.
func ExampleIn() {
	q := query.Select(query.Path("team"), query.Path("score")).
		Where(query.In("tier", "pro", "team")).
		OrderBy("score", query.Desc)

	result, err := q.Run(exampleDocSet())
	if err != nil {
		log.Fatal(err)
	}
	teams, _ := result.Column("team")
	scores, _ := result.Column("score")
	for row := range result.RowCount {
		fmt.Printf("%s %s\n", teams.Cells[row], scores.Cells[row])
	}
	// Output:
	// "data" 9
	// "infra" 7
	// "data" 4
}
