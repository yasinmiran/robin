package gui

import (
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
)

func TestCheckPrivate(t *testing.T) {
	for _, o := range []Options{{Curtain: true}, {Silent: true}, {Curtain: true, Silent: true}} {
		if err := checkPrivate(o); err == nil || !strings.Contains(err.Error(), "--attach") {
			t.Errorf("%+v on a spawned desktop: %v", o, err)
		}
	}
	for _, o := range []Options{{}, {Attach: ":0", Curtain: true}, {Attach: "auto", Silent: true}} {
		if err := checkPrivate(o); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	s, _ := startMessage(Options{Attach: ":0", Curtain: true, Silent: true}, &probe.Report{})
	if !s.Curtain || !s.Silent {
		t.Errorf("start %+v leaves the switches out", s)
	}
}

func TestPrivateNote(t *testing.T) {
	no := protocol.PrivateSwitch{Unsupported: "cannot"}
	for _, tc := range []struct {
		name string
		p    *protocol.Private
		want []string // in the note; none for no note
	}{
		{"spawned", nil, nil},
		{"both off", &protocol.Private{}, []string{"screen and speakers", "--curtain", "--silent", "in the page"}},
		{"curtain only", &protocol.Private{Silent: no}, []string{"screen shows", "--curtain"}},
		{"silent only", &protocol.Private{Curtain: no}, []string{"speakers play", "--silent"}},
		{"neither possible", &protocol.Private{Curtain: no, Silent: no}, nil},
		{"curtain on", &protocol.Private{Curtain: protocol.PrivateSwitch{On: true}}, nil},
		{"silent on", &protocol.Private{Silent: protocol.PrivateSwitch{On: true}}, nil},
	} {
		got := privateNote(&protocol.Ready{Private: tc.p})
		if (got == "") != (len(tc.want) == 0) {
			t.Errorf("%s: note %q", tc.name, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: note %q lacks %q", tc.name, got, w)
			}
		}
		if tc.name == "curtain only" && strings.Contains(got, "--silent") || tc.name == "silent only" && strings.Contains(got, "--curtain") {
			t.Errorf("%s: note %q offers what the desktop cannot do", tc.name, got)
		}
	}
}
