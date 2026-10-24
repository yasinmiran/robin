package protocol

import (
	"encoding/json"
	"testing"
)

func TestTextWireShape(t *testing.T) {
	b, err := json.Marshal(Control{T: ControlText, Text: "日本語\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"t":"text","text":"日本語\n"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	var m Control
	if err := json.Unmarshal([]byte(`{"t":"text","text":"é"}`), &m); err != nil || m.T != ControlText || m.Text != "é" {
		t.Errorf("decoded %+v, %v", m, err)
	}
}
