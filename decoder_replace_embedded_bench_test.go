package vibejson

import (
	"encoding/json"
	"reflect"
	"testing"
)

func BenchmarkDecodeEmbeddedReplace(b *testing.B) {
	for _, name := range []string{"unique_present", "shared_present", "shared_absent"} {
		b.Run(name, func(b *testing.B) {
			decoder := mustCompileTestDecoder[replaceEmbeddedAliasDocument](b, DecoderOptions{Replace: true})
			src := []byte(`{"x":1,"other":{"x":2}}`)
			got := replaceEmbeddedAliasDocument{ReplaceAliasEmbedded: &ReplaceAliasEmbedded{}, Other: &ReplaceAliasEmbedded{}}
			var shared *ReplaceAliasEmbedded
			if name != "unique_present" {
				shared = &ReplaceAliasEmbedded{}
				// Equal values let the pre-fix implementation pass the value
				// preflight too; alias independence has dedicated regressions.
				src = []byte(`{"x":2,"other":{"x":2}}`)
				if name == "shared_absent" {
					src = []byte(`{"other":{"x":0}}`)
				}
				got.ReplaceAliasEmbedded, got.Other = shared, shared
			}
			var want replaceEmbeddedAliasDocument
			if err := json.Unmarshal(src, &want); err != nil {
				b.Fatal(err)
			}
			if err := decoder.Decode(src, &got); err != nil || !reflect.DeepEqual(got, want) {
				b.Fatalf("decode preflight failed: got=%#v want=%#v err=%v", got, want, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if shared != nil {
					got.ReplaceAliasEmbedded, got.Other = shared, shared
					shared.X = 7
				}
				if err := decoder.Decode(src, &got); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	for _, replace := range []bool{false, true} {
		name := "ordinary_merge"
		if replace {
			name = "ordinary_replace"
		}
		b.Run(name, func(b *testing.B) {
			type document struct {
				X    int  `json:"x"`
				Flag bool `json:"flag"`
			}
			decoder := mustCompileTestDecoder[document](b, DecoderOptions{Replace: replace})
			src := []byte(`{"x":1,"flag":true}`)
			var got document
			if err := decoder.Decode(src, &got); err != nil || got != (document{X: 1, Flag: true}) {
				b.Fatalf("decode preflight failed: got=%#v err=%v", got, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := decoder.Decode(src, &got); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
