package jsonrepair

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRepairJson(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"raw newline", "{\"x\":\"line\nbreak\"}", "{\"x\":\"line\\nbreak\"}"},
		{"invalid escape", `{"x":"bad \x"}`, `{"x":"bad \\x"}`},
		{"valid unicode escape", `{"x":"\uD83D"}`, `{"x":"\uD83D"}`},
		{"already valid", `{"x":"ok","y":"\\n"}`, `{"x":"ok","y":"\\n"}`},
		{"trailing backslash", `{"x":"abc\`, `{"x":"abc\\`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepairJson(tc.input); got != tc.want {
				t.Fatalf("RepairJson() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseJsonWithRepair(t *testing.T) {
	var valid map[string]any
	if err := ParseJsonWithRepair(`{"x":1}`, &valid); err != nil {
		t.Fatalf("valid ParseJsonWithRepair: %v", err)
	}
	if valid["x"] != float64(1) {
		t.Fatalf("valid parse = %#v, want x=1", valid)
	}

	var repaired map[string]any
	if err := ParseJsonWithRepair("{\"x\":\"line\nbreak\"}", &repaired); err != nil {
		t.Fatalf("repairable ParseJsonWithRepair: %v", err)
	}
	if repaired["x"] != "line\nbreak" {
		t.Fatalf("repairable parse = %#v, want newline string", repaired)
	}

	input := `{`
	var original map[string]any
	originalErr := json.Unmarshal([]byte(input), &original)
	var got map[string]any
	err := ParseJsonWithRepair(input, &got)
	if err == nil {
		t.Fatalf("unrepairable ParseJsonWithRepair returned nil error")
	}
	if err.Error() != originalErr.Error() {
		t.Fatalf("unrepairable error = %q, want original %q", err.Error(), originalErr.Error())
	}
}

func TestParseStreamingJSON(t *testing.T) {
	empty := ParseStreamingJSON("")
	if empty == nil || len(empty) != 0 {
		t.Fatalf("ParseStreamingJSON(empty) = %#v, want non-nil empty map", empty)
	}

	valid := ParseStreamingJSON(`{"x":1}`)
	if valid["x"] != float64(1) {
		t.Fatalf("valid ParseStreamingJSON = %#v, want x=1", valid)
	}

	repairable := ParseStreamingJSON("{\"x\":\"line\nbreak\"}")
	if repairable["x"] != "line\nbreak" {
		t.Fatalf("repairable ParseStreamingJSON = %#v, want newline string", repairable)
	}

	garbage := ParseStreamingJSON("not json")
	if garbage == nil || !reflect.DeepEqual(garbage, map[string]any{}) {
		t.Fatalf("garbage ParseStreamingJSON = %#v, want non-nil empty map", garbage)
	}
}
