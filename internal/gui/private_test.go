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
}

func TestStartMessageCarriesTheSwitches(t *testing.T) {
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
		not  []string // not in it
	}{
		{"spawned", nil, nil, nil},
		{"both off", &protocol.Private{}, []string{"screen and speakers", "--curtain", "--silent", "in the page"}, nil},
		{"curtain only", &protocol.Private{Silent: no}, []string{"screen shows", "--curtain"}, []string{"--silent"}},
		{"silent only", &protocol.Private{Curtain: no}, []string{"speakers play", "--silent"}, []string{"--curtain"}},
		{"neither possible", &protocol.Private{Curtain: no, Silent: no}, nil, nil},
		{"curtain on", &protocol.Private{Curtain: protocol.PrivateSwitch{On: true}}, nil, nil},
		{"silent on", &protocol.Private{Silent: protocol.PrivateSwitch{On: true}}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privateNote(&protocol.Ready{Private: tc.p})
			if (got == "") != (len(tc.want) == 0) {
				t.Errorf("note %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("note %q lacks %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("note %q offers %q, which the desktop cannot do", got, n)
				}
			}
		})
	}
}
