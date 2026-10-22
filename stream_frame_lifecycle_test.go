package vibejson

import (
	"strings"
	"testing"
)

func TestValueFrameInitDiscardsPreviousState(t *testing.T) {
	for _, prior := range []struct {
		name     string
		src      string
		complete bool
	}{
		{"unfinished-string-escape", "\"\\", false},
		{"unfinished-container-string", `["open\`, false},
		{"unfinished-exponent", `1e`, false},
		{"invalid-exponent", `1e `, true},
		{"deep-unfinished-container", strings.Repeat("[", 128) + `["`, false},
		{"completed-string", `"hello\\world"`, true},
		{"completed-object", `{"a":"x"}`, true},
		{"completed-array", `[1]`, true},
		{"completed-literal", `true`, true},
		{"completed-number", `12 `, true},
	} {
		for _, next := range []struct {
			name string
			src  string
			end  int
		}{
			{"string", `""`, 2},
			{"object", `{"a":1}`, 7},
			{"array", `[]`, 2},
			{"null", `null true`, 4},
			{"false", `false[]`, 5},
			{"number", `1-2 `, 1},
			{"negative-number", `-2 `, 2},
		} {
			t.Run(prior.name+"/"+next.name, func(t *testing.T) {
				var frame ValueFrame
				previous := []byte(prior.src)
				frame.Init(previous[0])
				if complete := frame.Scan(previous, 0, len(previous)); complete != prior.complete {
					t.Fatalf("previous frame complete=%v; want %v", complete, prior.complete)
				}
				data := []byte(next.src)
				frame.Init(data[0])
				if complete := frame.Scan(data, 0, len(data)); !complete || frame.Framed != next.end {
					t.Fatalf("new frame complete=%v bytes=%d; want complete at %d", complete, frame.Framed, next.end)
				}
			})
		}
	}
}
