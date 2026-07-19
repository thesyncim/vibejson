package simdjson

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

type readCounter struct {
	in    io.Reader
	reads atomic.Int64
}

func (r *readCounter) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.in.Read(p)
}

func TestReaderLifecycleSequences(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		configure func(*testing.T, *Reader)
		wantNext  bool
		wantValue string
		wantErr   bool
		wantReads bool
	}{
		{
			name:  "set-limit-next",
			input: `"123456789"`,
			configure: func(t *testing.T, r *Reader) {
				if err := r.SetMaxValueBytes(4); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true, wantReads: true,
		},
		{
			name:  "close-next",
			input: "1\n",
			configure: func(t *testing.T, r *Reader) {
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "close-close-next",
			input: "1\n",
			configure: func(t *testing.T, r *Reader) {
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src := &readCounter{in: strings.NewReader(test.input)}
			r := NewReaderSize(src, 512)
			test.configure(t, r)
			if got := src.reads.Load(); got != 0 {
				t.Fatalf("configuration performed %d reads before Next", got)
			}
			if got := r.Next(); got != test.wantNext {
				t.Fatalf("Next = %v, want %v (Err = %v)", got, test.wantNext, r.Err())
			}
			if got := string(r.Bytes()); got != test.wantValue {
				t.Fatalf("Bytes = %q, want %q", got, test.wantValue)
			}
			if got := r.Err() != nil; got != test.wantErr {
				t.Fatalf("Err = %v, want error=%v", r.Err(), test.wantErr)
			}
			if got := src.reads.Load() > 0; got != test.wantReads {
				t.Fatalf("source read=%v, want %v", got, test.wantReads)
			}
		})
	}
}

func TestReaderRejectsConfigurationAfterStart(t *testing.T) {
	r := NewReader(strings.NewReader("1 22"))
	if !r.Next() || !bytes.Equal(r.Bytes(), []byte("1")) {
		t.Fatalf("first Next: Bytes=%q Err=%v", r.Bytes(), r.Err())
	}
	if err := r.SetMaxValueBytes(1); !errors.Is(err, ErrReaderStarted) {
		t.Fatalf("SetMaxValueBytes error = %v, want ErrReaderStarted", err)
	}
	if r.maxValue != 0 {
		t.Fatalf("rejected configuration changed maxValue to %d", r.maxValue)
	}
	if !r.Next() || !bytes.Equal(r.Bytes(), []byte("22")) {
		t.Fatalf("second Next: Bytes=%q Err=%v", r.Bytes(), r.Err())
	}
}

func TestReaderCloseClearsCurrentValue(t *testing.T) {
	dec, err := CompileDecoder[int](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(strings.NewReader("1 2"))
	if !r.Next() {
		t.Fatalf("first Next: %v", r.Err())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if r.Bytes() != nil {
		t.Fatalf("Bytes after Close = %q, want nil", r.Bytes())
	}
	if r.Next() {
		t.Fatal("Next succeeded after Close")
	}
	var dst int
	if DecodeNext(r, dec, &dst) {
		t.Fatal("DecodeNext succeeded after Close")
	}
	if err := DecodeFrom(r, dec, &dst); !errors.Is(err, ErrReaderClosed) {
		t.Fatalf("DecodeFrom error = %v, want ErrReaderClosed", err)
	}
}

func TestReaderCloseReleasesOwnedResources(t *testing.T) {
	value := `"` + strings.Repeat("x", 2048) + `"`
	source := strings.NewReader(value)
	r := NewReaderSize(source, 512)
	if !r.Next() {
		t.Fatalf("Next: %v", r.Err())
	}
	if cap(r.buf) <= 512 {
		t.Fatalf("buffer capacity = %d, want growth beyond initial capacity", cap(r.buf))
	}
	alias := r.Bytes()
	offset := r.InputOffset()
	streamErr := r.Err()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if r.buf != nil {
		t.Fatalf("Reader retained buffer with capacity %d", cap(r.buf))
	}
	if r.in != nil {
		t.Fatal("Reader retained input after Close")
	}
	if got := string(alias); got != value {
		t.Fatalf("caller-held alias changed after Close: got %q", got)
	}
	if got := r.InputOffset(); got != offset {
		t.Fatalf("InputOffset after Close = %d, want %d", got, offset)
	}
	if got := r.Err(); got != streamErr {
		t.Fatalf("Err after Close = %v, want %v", got, streamErr)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewReaderWithOptionsIsLazy(t *testing.T) {
	src := &readCounter{in: strings.NewReader(`"123456789"`)}
	r, err := NewReaderWithOptions(src, ReaderOptions{
		BufferSize:    512,
		MaxValueBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := src.reads.Load(); got != 0 {
		t.Fatalf("constructor performed %d reads", got)
	}
	if r.Next() || r.Err() == nil {
		t.Fatalf("oversized Next = true or nil error: %v", r.Err())
	}
}

func TestReaderConfigurationValidation(t *testing.T) {
	if _, err := NewReaderWithOptions(strings.NewReader("1"), ReaderOptions{BufferSize: -1}); err == nil {
		t.Fatal("negative buffer size accepted")
	}
	if _, err := NewReaderWithOptions(strings.NewReader("1"), ReaderOptions{MaxValueBytes: -1}); err == nil {
		t.Fatal("negative value limit accepted")
	}
	r := NewReader(strings.NewReader("1"))
	if err := r.SetMaxValueBytes(-1); err == nil {
		t.Fatal("negative value limit accepted by setter")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMaxValueBytes(1); !errors.Is(err, ErrReaderClosed) {
		t.Fatalf("SetMaxValueBytes after Close = %v, want ErrReaderClosed", err)
	}
}

// FuzzReaderLifecycleOperations checks arbitrary configuration, read, decode,
// and close sequences against the Reader's three-state contract. In
// particular, Close must be terminal and must never permit another source
// read, while configuration remains lazy before the first input operation.
func FuzzReaderLifecycleOperations(f *testing.F) {
	f.Add([]byte("1 2 3"), []byte{0, 1, 0, 3, 2, 1, 4})
	f.Add([]byte(`{"a":1}
[2,3]
true`), []byte{1, 4, 3, 5, 2, 0, 3})
	f.Add([]byte(`"unterminated`), []byte{0, 3, 0, 2, 1})

	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, data, operations []byte) {
		if len(data) > 1<<13 || len(operations) > 64 {
			t.Skip()
		}
		source := &readCounter{in: bytes.NewReader(data)}
		reader, err := NewReaderWithOptions(source, ReaderOptions{BufferSize: 512})
		if err != nil {
			t.Fatal(err)
		}
		state := readerConfigured
		readsAtClose := int64(-1)
		var decoded any

		for step, operation := range operations {
			switch operation % 6 {
			case 0:
				before := source.reads.Load()
				err := reader.SetMaxValueBytes(int(operation >> 3))
				switch state {
				case readerConfigured:
					if err != nil {
						t.Fatalf("step %d configured SetMaxValueBytes: %v", step, err)
					}
					if after := source.reads.Load(); after != before {
						t.Fatalf("step %d configuration read source: %d -> %d", step, before, after)
					}
				case readerStarted:
					if !errors.Is(err, ErrReaderStarted) {
						t.Fatalf("step %d started SetMaxValueBytes: %v", step, err)
					}
				case readerClosed:
					if !errors.Is(err, ErrReaderClosed) {
						t.Fatalf("step %d closed SetMaxValueBytes: %v", step, err)
					}
				}
			case 1:
				ok := reader.Next()
				if state == readerConfigured {
					state = readerStarted
				}
				if state == readerClosed && ok {
					t.Fatalf("step %d Next succeeded after Close", step)
				}
				if ok && !Valid(reader.Bytes()) {
					t.Fatalf("step %d Next returned invalid value %q", step, reader.Bytes())
				}
			case 2:
				if err := reader.Close(); err != nil {
					t.Fatalf("step %d Close: %v", step, err)
				}
				state = readerClosed
				if readsAtClose < 0 {
					readsAtClose = source.reads.Load()
				}
				if reader.Bytes() != nil {
					t.Fatalf("step %d Close retained current value %q", step, reader.Bytes())
				}
			case 3:
				ok := DecodeNext(reader, decoder, &decoded)
				if state == readerConfigured {
					state = readerStarted
				}
				if state == readerClosed && ok {
					t.Fatalf("step %d DecodeNext succeeded after Close", step)
				}
				if ok && !Valid(reader.Bytes()) {
					t.Fatalf("step %d DecodeNext exposed invalid value %q", step, reader.Bytes())
				}
			case 4:
				err := DecodeFrom(reader, decoder, &decoded)
				if state == readerClosed && !errors.Is(err, ErrReaderClosed) {
					t.Fatalf("step %d DecodeFrom after Close: %v", step, err)
				}
			case 5:
				if err := reader.Close(); err != nil {
					t.Fatalf("step %d first Close: %v", step, err)
				}
				if err := reader.Close(); err != nil {
					t.Fatalf("step %d second Close: %v", step, err)
				}
				state = readerClosed
				if readsAtClose < 0 {
					readsAtClose = source.reads.Load()
				}
			}
			if state == readerClosed {
				if got := source.reads.Load(); got != readsAtClose {
					t.Fatalf("step %d source reads after Close: %d -> %d", step, readsAtClose, got)
				}
				if reader.Bytes() != nil {
					t.Fatalf("step %d Bytes after Close = %q", step, reader.Bytes())
				}
			}
		}

		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		reads := source.reads.Load()
		if reader.Next() || DecodeNext(reader, decoder, &decoded) {
			t.Fatal("terminal reader produced a value")
		}
		if got := source.reads.Load(); got != reads {
			t.Fatalf("terminal operations read source: %d -> %d", reads, got)
		}
	})
}
