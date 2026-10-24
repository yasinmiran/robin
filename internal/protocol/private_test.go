package protocol

import (
	"encoding/json"
	"testing"
)

// The page builds and reads these by hand, so the wire form is the contract.
func TestPrivateWireForm(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{Control{T: ControlCurtain, On: true}, `{"t":"curtain","on":true}`},
		{Control{T: ControlSilent}, `{"t":"silent"}`},
		{Control{T: ControlPrivate, Private: &Private{
			Curtain: PrivateSwitch{On: true},
			Silent:  PrivateSwitch{Unsupported: "no sound server", Error: "refused"},
		}}, `{"t":"private","private":{"curtain":{"on":true},"silent":{"unsupported":"no sound server","error":"refused"}}}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%T marshals to\n%s\nwant\n%s", c.v, b, c.want)
		}
	}

	var r Ready
	if err := json.Unmarshal([]byte(`{"mode":"spawn"}`), &r); err != nil || r.Private != nil {
		t.Errorf("a ready without private reads as %+v, %v; want nil", r.Private, err)
	}
}

// Start carries the two switches only when they are on, so a CLI that has
// neither sends what an older agent knows.
func TestStartCarriesTheSwitches(t *testing.T) {
	keys := func(s Start) map[string]any {
		t.Helper()
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := keys(Start{Curtain: true, Silent: true}); m["curtain"] != true || m["silent"] != true {
		t.Errorf("Start with both on marshals curtain %v, silent %v", m["curtain"], m["silent"])
	}
	m := keys(Start{})
	if _, ok := m["curtain"]; ok {
		t.Error("a Start without the curtain carries it")
	}
	if _, ok := m["silent"]; ok {
		t.Error("a Start without the silent room carries it")
	}
}
