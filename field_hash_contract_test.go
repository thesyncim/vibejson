package vibejson

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Equal-length field names with an identical eight-byte prefix used to form
// one probe chain. Exercise the public decoder with reverse-order members.
type sharedPrefixRecord struct {
	Field0  int `json:"metadata_field_000"`
	Field1  int `json:"metadata_field_001"`
	Field2  int `json:"metadata_field_002"`
	Field3  int `json:"metadata_field_003"`
	Field4  int `json:"metadata_field_004"`
	Field5  int `json:"metadata_field_005"`
	Field6  int `json:"metadata_field_006"`
	Field7  int `json:"metadata_field_007"`
	Field8  int `json:"metadata_field_008"`
	Field9  int `json:"metadata_field_009"`
	Field10 int `json:"metadata_field_010"`
	Field11 int `json:"metadata_field_011"`
	Field12 int `json:"metadata_field_012"`
	Field13 int `json:"metadata_field_013"`
	Field14 int `json:"metadata_field_014"`
	Field15 int `json:"metadata_field_015"`
	Field16 int `json:"metadata_field_016"`
	Field17 int `json:"metadata_field_017"`
	Field18 int `json:"metadata_field_018"`
	Field19 int `json:"metadata_field_019"`
	Field20 int `json:"metadata_field_020"`
	Field21 int `json:"metadata_field_021"`
	Field22 int `json:"metadata_field_022"`
	Field23 int `json:"metadata_field_023"`
	Field24 int `json:"metadata_field_024"`
	Field25 int `json:"metadata_field_025"`
	Field26 int `json:"metadata_field_026"`
	Field27 int `json:"metadata_field_027"`
	Field28 int `json:"metadata_field_028"`
	Field29 int `json:"metadata_field_029"`
	Field30 int `json:"metadata_field_030"`
	Field31 int `json:"metadata_field_031"`
	Field32 int `json:"metadata_field_032"`
	Field33 int `json:"metadata_field_033"`
	Field34 int `json:"metadata_field_034"`
	Field35 int `json:"metadata_field_035"`
	Field36 int `json:"metadata_field_036"`
	Field37 int `json:"metadata_field_037"`
	Field38 int `json:"metadata_field_038"`
	Field39 int `json:"metadata_field_039"`
	Field40 int `json:"metadata_field_040"`
	Field41 int `json:"metadata_field_041"`
	Field42 int `json:"metadata_field_042"`
	Field43 int `json:"metadata_field_043"`
	Field44 int `json:"metadata_field_044"`
	Field45 int `json:"metadata_field_045"`
	Field46 int `json:"metadata_field_046"`
	Field47 int `json:"metadata_field_047"`
	Field48 int `json:"metadata_field_048"`
	Field49 int `json:"metadata_field_049"`
	Field50 int `json:"metadata_field_050"`
	Field51 int `json:"metadata_field_051"`
	Field52 int `json:"metadata_field_052"`
	Field53 int `json:"metadata_field_053"`
	Field54 int `json:"metadata_field_054"`
	Field55 int `json:"metadata_field_055"`
	Field56 int `json:"metadata_field_056"`
	Field57 int `json:"metadata_field_057"`
	Field58 int `json:"metadata_field_058"`
	Field59 int `json:"metadata_field_059"`
	Field60 int `json:"metadata_field_060"`
	Field61 int `json:"metadata_field_061"`
	Field62 int `json:"metadata_field_062"`
	Field63 int `json:"metadata_field_063"`
}

func sharedPrefixFields() ([]byte, *sharedPrefixRecord) {
	var src strings.Builder
	src.WriteByte('{')
	for i := 0; i < 64; i++ {
		if i != 0 {
			src.WriteByte(',')
		}
		fmt.Fprintf(&src, `"metadata_field_%03d":%d`, 63-i, 63-i)
	}
	src.WriteByte('}')
	return []byte(src.String()), new(sharedPrefixRecord)
}

func TestDecodeSharedPrefixFields(t *testing.T) {
	src, dst := sharedPrefixFields()
	if err := Unmarshal(src, dst); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		if got := reflect.ValueOf(dst).Elem().Field(i).Int(); got != int64(i) {
			t.Fatalf("field %d = %d", i, got)
		}
	}

	// Bound clustering rather than fixing an implementation-specific hash value.
	var slots [128]bool
	worst := 0
	for i := 0; i < 64; i++ {
		slot := fieldNameHash(fmt.Sprintf("metadata_field_%03d", i)) & 127
		probes := 1
		for slots[slot] {
			slot = (slot + 1) & 127
			probes++
		}
		slots[slot] = true
		if probes > worst {
			worst = probes
		}
	}
	if worst > 16 {
		t.Fatalf("shared-prefix field table has a %d-slot probe chain", worst)
	}
}

func BenchmarkDecodeSharedPrefixFields(b *testing.B) {
	src, dst := sharedPrefixFields()
	ptr := dst
	if err := Unmarshal(src, ptr); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		if err := Unmarshal(src, ptr); err != nil {
			b.Fatal(err)
		}
	}
}
