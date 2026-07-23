package query

import (
	"testing"

	"github.com/thesyncim/slopjson"
)

func TestPreparedPlanUnifiesBuilderAndSQL(t *testing.T) {
	docs := [][]byte{
		[]byte(`{"bucket":"a","score":3,"live":true}`),
		[]byte(`{"bucket":"b","score":7,"live":false}`),
		[]byte(`{"bucket":"a","score":5,"live":true}`),
	}
	set := buildDocSet(t, docs, storageMode{"", true, true})
	builder, err := Select(Path("bucket"), Count(), Sum("score")).
		Where(Cmp("live", Eq, true)).
		GroupBy("bucket").
		OrderBy("bucket", Asc).
		Prepare()
	if err != nil {
		t.Fatal(err)
	}
	sql, err := PrepareSQL(`SELECT bucket, COUNT(*), SUM(score) FROM docs WHERE live = true GROUP BY bucket ORDER BY bucket`)
	if err != nil {
		t.Fatal(err)
	}

	gotBuilder, err := builder.Run(set)
	if err != nil {
		t.Fatal(err)
	}
	gotSQL, err := sql.Run(set)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dumpResult(gotBuilder), dumpResult(gotSQL); got != want {
		t.Fatalf("prepared builder and SQL differ:\n%s\n%s", got, want)
	}
	for col := 1; col < len(gotBuilder.Columns); col++ {
		if gotBuilder.Columns[col].Cells[0].raw != nil {
			t.Fatalf("aggregate column %d retained eagerly formatted JSON", col)
		}
		buf := make([]byte, 0, 32)
		if got := gotBuilder.Columns[col].Cells[0].AppendJSON(buf); len(got) == 0 {
			t.Fatalf("aggregate column %d appended no JSON", col)
		}
	}

	schema := builder.AppendSchema(make([]OutputColumn, 0, 3))
	want := []OutputColumn{
		{Header: "bucket", Ordinal: 0, Reduction: ReductionNone, Type: TypeAny},
		{Header: "count(*)", Ordinal: 1, Reduction: ReductionCount, Type: TypeNumber},
		{Header: "sum(score)", Ordinal: 2, Reduction: ReductionSum, Type: TypeNumber},
	}
	if len(schema) != len(want) {
		t.Fatalf("schema length = %d, want %d", len(schema), len(want))
	}
	for i := range want {
		if schema[i] != want[i] {
			t.Fatalf("schema[%d] = %+v, want %+v", i, schema[i], want[i])
		}
	}

	allocs := testing.AllocsPerRun(100, func() {
		schema = builder.AppendSchema(schema[:0])
	})
	if allocs != 0 {
		t.Fatalf("warmed AppendSchema allocated %.2f times, want 0", allocs)
	}
}

func TestZeroPlanRejected(t *testing.T) {
	var p Plan
	var dst Result
	var workspace Workspace
	if err := p.RunInto(&dst, &slopjson.DocSet{}, &workspace); err == nil {
		t.Fatal("zero Plan RunInto succeeded")
	}
	if got := p.AppendSchema(nil); got != nil {
		t.Fatalf("zero Plan schema = %+v, want nil", got)
	}
}
