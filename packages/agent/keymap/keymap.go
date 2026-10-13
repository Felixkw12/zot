// Package keymap defines the persisted interactive keybinding format.
package keymap

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Binding is either a legacy slash-command string or a contextual action.
// Navigation actions are "next" and "previous" in context "navigation".
type Binding struct {
	Command string `json:"-"`
	Action  string `json:"action"`
	Context string `json:"context"`
}

// Map maps terminal chord names to bindings.
type Map map[string]Binding

// Commands converts legacy slash-command bindings to the persisted format.
func Commands(commands map[string]string) Map {
	bindings := make(Map, len(commands))
	for chord, command := range commands {
		bindings[chord] = Binding{Command: command}
	}
	return bindings
}

func (b *Binding) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("keymap binding must be a command string or action object")
	}
	*b = Binding{}
	if data[0] == '"' {
		return json.Unmarshal(data, &b.Command)
	}
	if data[0] != '{' {
		return fmt.Errorf("keymap binding must be a command string or action object")
	}
	type actionBinding Binding
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*actionBinding)(b))
}

func (b Binding) MarshalJSON() ([]byte, error) {
	if b.Action == "" && b.Context == "" {
		return json.Marshal(b.Command)
	}
	if b.Command != "" {
		return nil, fmt.Errorf("keymap binding cannot combine a command with an action")
	}
	type actionBinding Binding
	return json.Marshal(actionBinding(b))
}
