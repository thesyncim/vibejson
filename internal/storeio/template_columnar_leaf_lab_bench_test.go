package storeio

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
)

// Qualification command:
//   go test ./internal/storeio -run '^$' -bench '^BenchmarkTemplateColumnarLeafLab' -benchmem -benchtime=250ms -count=5
//
// Promotion gates are intentionally encoded in metric names rather than test
// failures: representative saving >=30%; selected adversarial overhead <=2%;
// splice <=45ns; field <=25ns; extraction increment <=15%. The checked-in
// verdict below must be updated from measured medians whenever the codec or Go
// toolchain changes.
//
// Apple M4 Max, Go 1.26.5, 190 rows, medians of five 250ms runs:
// low/high space 218.4 vs 258.4 B/doc (15.5% saved: FAIL >=30% gate);
// unique-shape selected raw at 273.8 B/doc (0% overhead: PASS <=2%);
// splice 93.43ns (FAIL <=45ns); field 14.03ns (PASS <=25ns);
// validation 138.1ns vs validation+extract 326.5ns, +136% (FAIL <=15%);
// admitted field patch + region/root reseal 140.9ns vs whole-leaf reseal
// 3640ns (25.8x faster). Every operation reports 0 allocs/op. These are
// qualification results, not product claims.

var (
	templateColumnarLeafLabBenchBytes []byte
	templateColumnarLeafLabBenchKind  uint8
	templateColumnarLeafLabBenchBool  bool
	templateColumnarLeafLabBenchView  TemplateColumnarLeafLabView
	templateColumnarLeafLabBenchExt   TemplateColumnarLeafLabExtraction
)

func templateColumnarLeafLabCompetitiveRows(
	count int, high, uniqueShape bool,
) []TemplateColumnarLeafLabRow {
	rng := rand.New(rand.NewSource(0x5deece66d))
	rows := make([]TemplateColumnarLeafLabRow, count)
	notes := []string{
		"steady state, no anomalies observed in the last reporting window",
		"migrated from the legacy pipeline during the maintenance window",
		"flagged for review after a threshold breach on the ingest path",
		"nominal; retention policy applied and checkpoint acknowledged",
	}
	randomText := func(n int) string {
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = byte('a' + rng.Intn(26))
		}
		return string(buf)
	}
	for i := range rows {
		tier, region, note := "team", "eu-west-1", notes[i%len(notes)]
		if high {
			tier, region, note = randomText(len(tier)), randomText(len(region)), randomText(len(note))
		}
		shape := ""
		if uniqueShape {
			shape = fmt.Sprintf(`,"shape_%03d":%d`, i, i)
		}
		json := fmt.Appendf(nil,
			`{"id":%d,"name":"user-%d","country":"%s","score":%d,"active":%t,`+
				`"profile":{"tier":"%s","region":"%s","joined":"2024-07-%02d"},`+
				`"tags":["alpha","beta","gamma"],"note":"%s"%s}`,
			i, i, []string{"PT", "ES", "FR", "DE"}[i&3], i%1000, i%3 != 0,
			tier, region, 1+i%28, note, shape)
		rows[i] = TemplateColumnarLeafLabRow{
			Slot: uint8((i*197 + 17) & 255),
			Key:  []byte(fmt.Sprintf("doc:%08d", i)),
			JSON: json,
		}
	}
	return rows
}

func BenchmarkTemplateColumnarLeafLabSpace(b *testing.B) {
	for _, tc := range []struct {
		name         string
		high, unique bool
	}{
		{name: "CompetitiveLow"},
		{name: "CompetitiveHigh", high: true},
		{name: "AdversarialUniqueShape", high: true, unique: true},
	} {
		rows := templateColumnarLeafLabCompetitiveRows(190, tc.high, tc.unique)
		image, err := EncodeTemplateColumnarLeafLab(rows)
		if err != nil {
			b.Fatal(err)
		}
		raw := TemplateColumnarLeafLabRawBytes(rows)
		selected := min(raw, len(image))
		saving := float64(raw-selected) / float64(raw) * 100
		overhead := float64(selected-raw) / float64(raw) * 100
		if overhead < 0 {
			overhead = 0
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportMetric(float64(raw)/float64(len(rows)), "raw-B/doc")
			b.ReportMetric(float64(len(image))/float64(len(rows)), "template-B/doc")
			b.ReportMetric(float64(selected)/float64(len(rows)), "selected-B/doc")
			b.ReportMetric(saving, "selected-saving-%")
			b.ReportMetric(overhead, "selected-overhead-%")
		})
	}
}

func BenchmarkTemplateColumnarLeafLabAppendRaw(b *testing.B) {
	rows := templateColumnarLeafLabCompetitiveRows(190, true, false)
	image, err := EncodeTemplateColumnarLeafLab(rows)
	if err != nil {
		b.Fatal(err)
	}
	view, err := OpenTemplateColumnarLeafLab(image)
	if err != nil {
		b.Fatal(err)
	}
	row := rows[95]
	dst := make([]byte, 0, len(row.JSON))
	b.Run("TemplateSplice", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			templateColumnarLeafLabBenchBytes,
				templateColumnarLeafLabBenchBool = view.AppendRaw(dst[:0], row.Slot, row.Key)
		}
	})
	b.Run("RawLeafCopy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			templateColumnarLeafLabBenchBytes = append(dst[:0], row.JSON...)
		}
	})
}

func BenchmarkTemplateColumnarLeafLabField(b *testing.B) {
	rows := templateColumnarLeafLabCompetitiveRows(190, true, false)
	image, err := EncodeTemplateColumnarLeafLab(rows)
	if err != nil {
		b.Fatal(err)
	}
	view, err := OpenTemplateColumnarLeafLab(image)
	if err != nil {
		b.Fatal(err)
	}
	slot := rows[95].Slot
	b.ReportAllocs()
	for b.Loop() {
		var kind document.Kind
		templateColumnarLeafLabBenchBytes, kind,
			templateColumnarLeafLabBenchBool = view.Field(slot, 3)
		templateColumnarLeafLabBenchKind = uint8(kind)
	}
}

func BenchmarkTemplateColumnarLeafLabExtraction(b *testing.B) {
	row := templateColumnarLeafLabCompetitiveRows(1, true, false)[0]
	storage := make([]vibejson.IndexEntry, len(row.JSON)+2)
	scratch := TemplateColumnarLeafLabExtraction{
		Skeleton: make([]byte, 0, len(row.JSON)),
		Holes:    make([]TemplateColumnarLeafLabHole, 0, 32),
		Fields:   make([][]byte, 0, 32),
	}
	b.Run("ValidationOnly", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := vibejson.BuildIndex(row.JSON, storage); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ValidationPlusExtraction", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var err error
			templateColumnarLeafLabBenchExt, err =
				ExtractTemplateColumnarLeafLabInto(row.JSON, storage, &scratch)
			if err != nil {
				b.Fatal(err)
			}
			scratch = templateColumnarLeafLabBenchExt
		}
	})
}

func BenchmarkTemplateColumnarLeafLabReseal(b *testing.B) {
	rows := templateColumnarLeafLabCompetitiveRows(190, false, false)
	original, err := EncodeTemplateColumnarLeafLab(rows)
	if err != nil {
		b.Fatal(err)
	}
	slot := rows[10].Slot // score is two bytes and replacement preserves width.
	b.Run("FieldPatchRegionAndRoot", func(b *testing.B) {
		image := append([]byte(nil), original...)
		view, err := OpenTemplateColumnarLeafLab(image)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.SetBytes(2)
		for b.Loop() {
			if err := patchTemplateColumnarLeafLabFieldFixedAdmitted(
				view, slot, 3, []byte("11"),
			); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("WholeLeafReseal", func(b *testing.B) {
		image := append([]byte(nil), original...)
		b.ReportAllocs()
		b.SetBytes(int64(len(image)))
		for b.Loop() {
			if err := ResealTemplateColumnarLeafLab(image); err != nil {
				b.Fatal(err)
			}
		}
	})
}
