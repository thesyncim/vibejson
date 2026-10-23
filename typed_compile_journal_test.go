package vibejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Each named map level reserves its own encoder key and value storage.
// Seventeen levels force journal entries beyond both an eight- and a
// sixteen-entry inline prefix.
type compileJournalMap01[T any] map[string]T
type compileJournalMap02[T any] map[string]compileJournalMap01[T]
type compileJournalMap03[T any] map[string]compileJournalMap02[T]
type compileJournalMap04[T any] map[string]compileJournalMap03[T]
type compileJournalMap05[T any] map[string]compileJournalMap04[T]
type compileJournalMap06[T any] map[string]compileJournalMap05[T]
type compileJournalMap07[T any] map[string]compileJournalMap06[T]
type compileJournalMap08[T any] map[string]compileJournalMap07[T]
type compileJournalMap09[T any] map[string]compileJournalMap08[T]
type compileJournalMap10[T any] map[string]compileJournalMap09[T]
type compileJournalMap11[T any] map[string]compileJournalMap10[T]
type compileJournalMap12[T any] map[string]compileJournalMap11[T]
type compileJournalMap13[T any] map[string]compileJournalMap12[T]
type compileJournalMap14[T any] map[string]compileJournalMap13[T]
type compileJournalMap15[T any] map[string]compileJournalMap14[T]
type compileJournalMap16[T any] map[string]compileJournalMap15[T]
type compileJournalMap17[T any] map[string]compileJournalMap16[T]

type compileJournalScalar int64

func (v compileJournalScalar) MarshalJSON() ([]byte, error) {
	return strconv.AppendInt(nil, int64(v)+100, 10), nil
}

func (v *compileJournalScalar) UnmarshalJSON(src []byte) error {
	var number int64
	if err := json.Unmarshal(src, &number); err != nil {
		return err
	}
	*v = compileJournalScalar(number - 100)
	return nil
}

type compileJournalHook[T any] struct {
	Value       T
	Unsupported chan int
	Seen        bool `json:"-"`
}

func (compileJournalHook[T]) MarshalJSON() ([]byte, error) {
	return []byte(`"hook"`), nil
}

func (h *compileJournalHook[T]) UnmarshalJSON(src []byte) error {
	var marker string
	if err := json.Unmarshal(src, &marker); err != nil {
		return err
	}
	if marker != "hook" {
		return errors.New("unexpected journal hook marker")
	}
	h.Seen = true
	return nil
}

type compileJournalTarget = compileJournalMap17[compileJournalScalar]
type compileJournalControl = compileJournalMap17[uint64]

func compileJournalJSON(number string) string {
	return strings.Repeat(`{"key":`, 17) + number + strings.Repeat(`}`, 17)
}

func checkCompileJournalPublic[T any](t *testing.T, src []byte) {
	t.Helper()
	var want T
	requireNoTestError(t, json.Unmarshal(src, &want))
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[T](t, DecoderOptions{Replace: replace})
		var got T
		for range 2 {
			requireNoTestError(t, decoder.Decode(src, &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("replace=%v: recovered graph decoded %#v, want %#v", replace, got, want)
			}
		}
	}
	encoded, err := json.Marshal(&want)
	requireNoTestError(t, err)
	encoder := mustCompileTestEncoder[T](t, EncoderOptions{})
	const prefix = "pre:"
	buffer := []byte(prefix)
	for range 2 {
		buffer, err = encoder.AppendJSON(buffer[:len(prefix)], &want)
		requireNoTestError(t, err)
		if !bytes.Equal(buffer, append([]byte(prefix), encoded...)) {
			t.Fatalf("recovered graph encoded %s, want %s%s", buffer, prefix, encoded)
		}
	}
}

func TestCompileJournalRecoveryAcrossInlineBoundary(t *testing.T) {
	type document struct {
		Hook  compileJournalHook[compileJournalTarget] `json:"hook"`
		After compileJournalTarget                     `json:"after"`
	}
	// The hook's checkpoint is inside the inline prefix, but its structural
	// children overflow it before the unsupported channel triggers recovery.
	// After must rebuild every removed map and custom scalar resource slot.
	src := []byte(`{"hook":"hook","after":` + compileJournalJSON("123") + `}`)
	checkCompileJournalPublic[document](t, src)
}

func TestCompileJournalRecoveryAfterOverflowPrefix(t *testing.T) {
	type document struct {
		Before compileJournalControl                    `json:"before"`
		Hook   compileJournalHook[compileJournalTarget] `json:"hook"`
		After  compileJournalTarget                     `json:"after"`
		Again  compileJournalControl                    `json:"again"`
	}
	// The disjoint control graph fills the journal beyond its inline prefix
	// before the failing hook starts. Recovery must preserve those earlier
	// resources while rebuilding the later target graph into valid slots.
	src := []byte(`{"before":` + compileJournalJSON("456") + `,"hook":"hook","after":` + compileJournalJSON("123") + `,"again":` + compileJournalJSON("789") + `}`)
	checkCompileJournalPublic[document](t, src)
}

func TestCompileJournalRollbackPreservesEarlierCompiles(t *testing.T) {
	type failedDocument struct {
		Hook compileJournalHook[compileJournalTarget] `json:"hook"`
		Bad  chan int                                 `json:"bad"`
	}
	for _, mode := range []typedCompileMode{typedCompileDecode, typedCompileEncode} {
		compiler := newTypedCompiler(mode)
		controlType := reflect.TypeFor[compileJournalControl]()
		_, err := compiler.compile(controlType, controlType.String())
		requireNoTestError(t, err)
		earlier := make(map[reflect.Type]*typedNode, len(compiler.nodes))
		for typ, node := range compiler.nodes {
			earlier[typ] = node
		}
		scratch := append([]reflect.Type(nil), compiler.encScratchTypes...)
		backing, hadMap := compiler.encBackingSlots, compiler.encHasMap

		failedType := reflect.TypeFor[failedDocument]()
		_, err = compiler.compile(failedType, failedType.String())
		var unsupported *UnsupportedTypeError
		if !errors.As(err, &unsupported) || unsupported.Type != reflect.TypeFor[chan int]() || unsupported.Path != failedType.String()+".bad" {
			t.Fatalf("mode=%v: outer failure = %v, want unsupported channel at .bad", mode, err)
		}
		if len(compiler.nodes) != len(earlier) {
			t.Fatalf("mode=%v: failed root retained %d nodes, want %d", mode, len(compiler.nodes), len(earlier))
		}
		for typ, node := range earlier {
			if compiler.nodes[typ] != node {
				t.Fatalf("mode=%v: failed root replaced earlier node %v", mode, typ)
			}
		}
		if !reflect.DeepEqual(compiler.encScratchTypes, scratch) || compiler.encBackingSlots != backing || compiler.encHasMap != hadMap {
			t.Fatalf("mode=%v: failed root changed earlier encoder resources", mode)
		}

		// A subsequent compile on the same compiler must succeed, remain
		// cached, and leave the prior successful graph in place.
		targetType := reflect.TypeFor[compileJournalTarget]()
		target, err := compiler.compile(targetType, targetType.String())
		requireNoTestError(t, err)
		again, err := compiler.compile(targetType, targetType.String())
		requireNoTestError(t, err)
		if again != target {
			t.Fatalf("mode=%v: later successful root was not cached", mode)
		}
		for typ, node := range earlier {
			if compiler.nodes[typ] != node {
				t.Fatalf("mode=%v: later compile replaced earlier node %v", mode, typ)
			}
		}
	}
}
