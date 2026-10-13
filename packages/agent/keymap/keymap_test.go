package keymap

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMapRoundTrip(t *testing.T) {
	input := []byte(`{"ctrl+s":"/settings","ctrl+n":{"action":"next","context":"navigation"},"ctrl+p":{"action":"previous","context":"navigation"}}`)
	var got Map
	if err := json.Unmarshal(input, &got); err != nil {
		t.Fatal(err)
	}
	want := Map{
		"ctrl+s": {Command: "/settings"},
		"ctrl+n": {Action: "next", Context: "navigation"},
		"ctrl+p": {Action: "previous", Context: "navigation"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bindings = %#v, want %#v", got, want)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var again Map
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("round trip = %#v, want %#v", again, want)
	}
	var shapes map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &shapes); err != nil {
		t.Fatal(err)
	}
	if string(shapes["ctrl+s"]) != `"/settings"` {
		t.Fatalf("legacy command no longer encoded as a string: %s", encoded)
	}
}

func TestBindingRejectsMalformedValues(t *testing.T) {
	for _, input := range []string{
		`null`, `true`, `42`, `[]`,
		`{"action":42,"context":"navigation"}`,
		`{"action":"next","context":"navigation","typo":true}`,
		`{"command":"/help","action":"next","context":"navigation"}`,
	} {
		var got Binding
		if err := json.Unmarshal([]byte(input), &got); err == nil {
			t.Fatalf("accepted invalid binding %s", input)
		}
	}
}

func TestBindingRejectsCommandActionCombination(t *testing.T) {
	_, err := json.Marshal(Binding{Command: "/help", Action: "next", Context: "navigation"})
	if err == nil {
		t.Fatal("encoded a binding that combined a command and action")
	}
}
