package gui

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/macros"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/web"
)

func macroBridge(t *testing.T, keeper macroKeeper) (*web.Server, *fakeAgent) {
	t.Helper()
	s, ctx := testServer(t)
	b := newBridge(nil, relay.New(relay.Policy{Transport: relay.Direct}),
		protocol.Config{ICEServers: []protocol.ICEServer{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if keeper != nil {
		b.macros = keeper
	}
	go b.accept(ctx, s)
	t.Cleanup(b.close)
	agent := newFakeAgent()
	runBridge(ctx, b, agent)
	return s, agent
}

func TestThePageKeepsItsMacrosThroughTheCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robin", "macros.json")
	s, agent := macroBridge(t, macros.Open(path, "alice@box"))

	pg := openPage(t, s)
	m := pg.recv()
	if m.Type != protocol.TypeConfig || m.Config.Macros == nil || m.Config.Macros.Host != "alice@box" || len(m.Config.Macros.List) != 0 {
		t.Fatalf("config %+v, want the host and no macros", m.Config)
	}

	lock := protocol.Macro{ID: "lock", Name: "Lock", Host: "alice@box", Steps: []protocol.MacroStep{
		{Keys: []protocol.MacroKey{{Key: "Meta", Code: "MetaLeft"}, {Key: "l", Code: "KeyL"}}},
	}}
	pg.send(protocol.Message{Type: protocol.TypeMacros, MacroChange: &protocol.MacroChange{Put: &lock}})
	m = pg.recv()
	if m.Type != protocol.TypeMacros || m.Macros == nil || m.Macros.Error != "" || len(m.Macros.List) != 1 || m.Macros.List[0].Name != "Lock" {
		t.Fatalf("after a put the page got %+v", m.Macros)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no file written: %v", err)
	}

	bad := lock
	bad.ID, bad.Name = "bad", ""
	pg.send(protocol.Message{Type: protocol.TypeMacros, MacroChange: &protocol.MacroChange{Put: &bad}})
	if m = pg.recv(); m.Macros == nil || !strings.Contains(m.Macros.Error, "name") || len(m.Macros.List) != 1 {
		t.Fatalf("a refused put: %+v", m.Macros)
	}
	pg.send(protocol.Message{Type: protocol.TypeMacros})
	if m = pg.recv(); m.Macros == nil || m.Macros.Error != "a change puts or deletes one macro" {
		t.Fatalf("no change: %+v", m.Macros)
	}

	// A reload finds the macro: the next Config reads the file again.
	pg.c.CloseNow()
	pg = reopenPage(t, s)
	if m = pg.recv(); m.Type != protocol.TypeConfig || m.Config.Macros == nil || len(m.Config.Macros.List) != 1 {
		t.Fatalf("config after a reload %+v", m.Config)
	}

	// None of it reached the agent: only the first page's hangup did.
	if m := sent(t, agent); m.Type != protocol.TypeHangup {
		t.Fatalf("the agent got %+v", m)
	}
	if len(agent.sent) != 0 {
		t.Fatalf("the agent got %+v", <-agent.sent)
	}
}

func TestASessionWithoutAKeeperRefusesChanges(t *testing.T) {
	s, _ := macroBridge(t, nil)
	pg := openPage(t, s)
	if m := pg.recv(); m.Config == nil || m.Config.Macros != nil {
		t.Fatalf("config %+v, want no macros", m.Config)
	}
	pg.send(protocol.Message{Type: protocol.TypeMacros, MacroChange: &protocol.MacroChange{Delete: "x"}})
	if m := pg.recv(); m.Type != protocol.TypeMacros || m.Macros == nil || m.Macros.Error != "this session keeps no macros" || m.Macros.List == nil {
		t.Fatalf("got %+v", m.Macros)
	}
}

func TestAMacrosFileThatDoesNotReadIsReportedOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.Mkdir(filepath.Join(dir, "robin"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "robin", "macros.json")
	if err := os.WriteFile(path, []byte("{\x1b[2J"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	s := openMacros("box", printableWriter{&stderr})
	out := stderr.String()
	if !strings.HasPrefix(out, "robin: macros: "+path+": ") || !strings.Contains(out, "leaves it as it is") || strings.Count(out, "\n") != 1 {
		t.Errorf("stderr %q", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a control character from the file reached the terminal: %q", out)
	}
	if got := s.Load(); got.Error == "" {
		t.Errorf("the page is not told")
	}

	stderr.Reset()
	if err := os.WriteFile(path, []byte(`{"version":1,"macros":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	openMacros("box", &stderr)
	if stderr.Len() != 0 {
		t.Errorf("a file that reads: %q", stderr.String())
	}
}
