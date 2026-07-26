package storeio

import "testing"

func BenchmarkMaterializationJournalEncodeTwoTinyPages(b *testing.B) {
	fixture := newMaterializationJournalFixture(b, 1000)
	slot := make([]byte, MaterializationJournalSize)
	b.ReportAllocs()
	b.SetBytes(MaterializationJournalSize)
	b.ReportMetric(float64(2048), "undo-B/op")
	b.ResetTimer()
	for range b.N {
		if _, err := EncodeMaterializationJournal(slot, fixture.header, fixture.targets, fixture.patches); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaterializationJournalOpenTwoTinyPages(b *testing.B) {
	fixture := newMaterializationJournalFixture(b, 1001)
	slot := encodeMaterializationFixture(b, fixture)
	b.ReportAllocs()
	b.SetBytes(MaterializationJournalSize)
	b.ResetTimer()
	for range b.N {
		if _, err := OpenMaterializationJournal(slot); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaterializationJournalRollbackTinyPageUpdate(b *testing.B) {
	fixture := newMaterializationJournalFixture(b, 1002)
	view, err := OpenMaterializationJournal(encodeMaterializationFixture(b, fixture))
	if err != nil {
		b.Fatal(err)
	}
	page := make([]byte, len(fixture.before[0]))
	b.ReportAllocs()
	b.SetBytes(int64(len(page)))
	b.ReportMetric(float64(len(fixture.patches[0].Data)+len(fixture.patches[1].Data)), "undo-B/op")
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		copy(page, fixture.after[0])
		b.StartTimer()
		if _, err := view.Rollback(0, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaterializationJournalCommittedRootNoPageRead(b *testing.B) {
	fixture := newMaterializationJournalFixture(b, 1003)
	view, err := OpenMaterializationJournal(encodeMaterializationFixture(b, fixture))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, err := view.RecoverTarget(fixture.header.TargetGeneration, -1, nil)
		if err != nil || result != MaterializationRollbackNotNeeded {
			b.Fatalf("RecoverTarget=(%d,%v)", result, err)
		}
	}
}
