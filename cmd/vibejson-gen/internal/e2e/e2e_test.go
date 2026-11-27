package e2e

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/thesyncim/vibejson"
)

func sampleOrder() Order {
	return Order{
		ID:       42,
		Active:   true,
		Customer: "Ada Lovelace",
		Note:     "deliver before 5pm <urgent> & signed",
		Scores:   [3]float64{1.5, 2.25, -3e4},
		Tags:     []string{"vip", "express", "fragile"},
		Shipping: &Address{Street: "123 Main St", City: "London", Zip: 10001},
		Billing:  Address{Street: "456 High Rd", City: "Bristol"},
		Ignored:  "must-not-appear",
		private:  99,
	}
}

func TestGeneratedCodecMatchesStdlib(t *testing.T) {
	cases := []Order{
		sampleOrder(),
		{ID: 0, Active: false, Customer: "", Billing: Address{}},
		{ID: -7, Customer: "quoted \"name\"", Tags: []string{}, Shipping: &Address{Street: "A", City: "B"}},
	}
	if cases[0].private != 99 {
		t.Fatalf("unexpected private field value: %d", cases[0].private)
	}

	enc, err := vibejson.CompileEncoder[Order](vibejson.EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := vibejson.CompileDecoder[Order](vibejson.DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for i := range cases {
		in := cases[i]
		wantJSON, err := json.Marshal(&in)
		if err != nil {
			t.Fatalf("case %d stdlib marshal: %v", i, err)
		}
		gotJSON, err := enc.AppendJSON(nil, &in)
		if err != nil {
			t.Fatalf("case %d vibejson marshal: %v", i, err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("case %d marshal mismatch:\ngot:  %s\nwant: %s", i, gotJSON, wantJSON)
		}

		var got, want Order
		if err := json.Unmarshal(wantJSON, &want); err != nil {
			t.Fatalf("case %d stdlib unmarshal: %v", i, err)
		}
		if err := dec.Decode(wantJSON, &got); err != nil {
			t.Fatalf("case %d vibejson unmarshal: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d unmarshal mismatch:\ngot:  %+v\nwant: %+v", i, got, want)
		}
	}

	// Out-of-order keys, unknown fields, and null pointers/slices.
	shuffled := []byte(`{"unknown":[1,{"x":true}],"billing":{"city":"Paris","street":"Rue 1"},"shipping":null,"tags":null,"scores":[9.5,8],"customer":"Grace","active":false,"id":7}`)
	var got, want Order
	got.Scores = [3]float64{1, 2, 3}
	want.Scores = [3]float64{1, 2, 3}
	got.Shipping = &Address{Street: "old"}
	want.Shipping = &Address{Street: "old"}
	got.Tags = []string{"old"}
	want.Tags = []string{"old"}

	if err := json.Unmarshal(shuffled, &want); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(shuffled, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shuffled decode mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func BenchmarkGeneratedOrderEncode(b *testing.B) {
	v := sampleOrder()
	enc, err := vibejson.CompileEncoder[Order](vibejson.EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	buf, err := enc.AppendJSON(nil, &v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buf, err = enc.AppendJSON(buf[:0], &v)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGeneratedOrderEncodeStdlib(b *testing.B) {
	v := sampleOrder()
	out, err := json.Marshal(&v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err = json.Marshal(&v)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGeneratedOrderDecode(b *testing.B) {
	v := sampleOrder()
	src, err := json.Marshal(&v)
	if err != nil {
		b.Fatal(err)
	}
	dec, err := vibejson.CompileDecoder[Order](vibejson.DecoderOptions{ZeroCopy: true})
	if err != nil {
		b.Fatal(err)
	}
	var dst Order
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := dec.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGeneratedOrderDecodeStdlib(b *testing.B) {
	v := sampleOrder()
	src, err := json.Marshal(&v)
	if err != nil {
		b.Fatal(err)
	}
	var dst Order
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := json.Unmarshal(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}
