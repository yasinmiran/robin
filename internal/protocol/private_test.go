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
		{Start{Curtain: true, Silent: true}, `{"version":0,"width":0,"height":0,"maxWidth":0,"maxHeight":0,"fps":0,"maxKbps":0,"curtain":true,"silent":true}`},
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
