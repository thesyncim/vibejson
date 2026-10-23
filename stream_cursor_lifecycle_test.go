package vibejson

import (
	"strings"
	"testing"
)

func TestValueCursorFinishWithoutCurrentValue(t *testing.T) {
	t.Run("zero cursor", func(t *testing.T) {
		var cursor ValueCursor
		if err := cursor.Finish(); err == nil {
			t.Fatal("Finish accepted a zero cursor")
		}
	})
	t.Run("before Next", func(t *testing.T) {
		reader := NewReader(strings.NewReader("42"))
		cursor := reader.Cursor()
		if err := cursor.Finish(); err == nil {
			t.Fatal("Finish accepted a cursor before Next")
		}
	})
	t.Run("after EOF", func(t *testing.T) {
		reader := NewReader(strings.NewReader("42"))
		if !reader.Next() {
			t.Fatalf("Next: %v", reader.Err())
		}
		if reader.Next() || reader.Err() != nil {
			t.Fatalf("expected clean EOF, got %v", reader.Err())
		}
		cursor := reader.Cursor()
		if err := cursor.Finish(); err == nil {
			t.Fatal("Finish accepted a cursor after EOF")
		}
	})
	t.Run("after Close", func(t *testing.T) {
		reader := NewReader(strings.NewReader("42"))
		if !reader.Next() {
			t.Fatalf("Next: %v", reader.Err())
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		cursor := reader.Cursor()
		if err := cursor.Finish(); err == nil {
			t.Fatal("Finish accepted a cursor after Close")
		}
	})
}

func TestValueCursorFinishRequiresConsumption(t *testing.T) {
	for _, src := range []string{"42", `"text"`, "null", "[]", `{"value":[1,2]}`} {
		t.Run(src, func(t *testing.T) {
			reader := NewReader(strings.NewReader(src))
			if !reader.Next() {
				t.Fatalf("Next: %v", reader.Err())
			}
			cursor := reader.Cursor()
			if err := cursor.Finish(); err == nil {
				t.Fatal("Finish accepted an unconsumed value")
			}
			if err := cursor.Skip(); err != nil {
				t.Fatalf("Skip: %v", err)
			}
			for range 2 {
				if err := cursor.Finish(); err != nil {
					t.Fatalf("Finish rejected a consumed value: %v", err)
				}
			}
		})
	}
}
