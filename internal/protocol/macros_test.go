package protocol

import (
	"encoding/json"
	"testing"
)

// The page builds these by hand, so their JSON is the contract.
func TestMacrosWireShape(t *testing.T) {
	put := Macro{ID: "m1", Name: "Lock", Host: "box", Steps: []MacroStep{
		{Keys: []MacroKey{{Key: "Meta", Code: "MetaLeft"}, {Key: "l", Code: "KeyL"}}},
		{Text: "hi\n"},
		{Pause: 250},
	}}
	b, err := json.Marshal(Message{Type: TypeMacros, MacroChange: &MacroChange{Put: &put}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"macros","macroChange":{"put":{"id":"m1","name":"Lock","host":"box","steps":[` +
		`{"keys":[{"key":"Meta","code":"MetaLeft"},{"key":"l","code":"KeyL"}]},{"text":"hi\n"},{"pause":250}]}}}`
	if string(b) != want {
		t.Errorf("put:\n got %s\nwant %s", b, want)
	}

	b, _ = json.Marshal(Message{Type: TypeMacros, MacroChange: &MacroChange{Delete: "m1"}})
	if got := string(b); got != `{"type":"macros","macroChange":{"delete":"m1"}}` {
		t.Errorf("delete: %s", got)
	}

	b, _ = json.Marshal(Config{ICEServers: []ICEServer{}, Macros: &Macros{Host: "box", List: []Macro{}, Error: "unreadable", ReadOnly: true}})
	if got := string(b); got != `{"iceServers":[],"macros":{"host":"box","list":[],"error":"unreadable","readOnly":true}}` {
		t.Errorf("config: %s", got)
	}
}
