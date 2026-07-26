package competitive

import (
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/store/durable"
)

func TestVibeDurableCompactBulkIsExplicitAndCannotBePutReplay(t *testing.T) {
	engine, err := newVibeDurable(Config{})
	if err != nil {
		t.Fatal(err)
	}
	verbatim := engine.(*vibeDurable)
	if got := verbatim.options().DocumentFormat; got != durable.DocumentFormatVerbatim {
		t.Fatalf("default document format = %d, want verbatim", got)
	}
	if strings.Contains(verbatim.Tuning(), "DocumentFormatCompact") {
		t.Fatalf("default tuning falsely reports compact: %q", verbatim.Tuning())
	}

	engine, err = newVibeDurable(Config{Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	compact := engine.(*vibeDurable)
	if got := compact.options().DocumentFormat; got != durable.DocumentFormatCompact {
		t.Fatalf("explicit document format = %d, want compact", got)
	}
	if !strings.Contains(compact.Tuning(), "DocumentFormatCompact") {
		t.Fatalf("compact tuning omits format: %q", compact.Tuning())
	}

	if _, err := newVibeDurable(Config{
		Compact: true,
		PutLoop: true,
	}); err == nil {
		t.Fatal("compact Put replay was accepted")
	}
}
