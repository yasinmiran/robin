//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/agent/files"
)

func TestSendCommand(t *testing.T) {
	root := t.TempDir()
	ctx, interrupt := context.WithCancel(t.Context())
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := send(ctx, args, root, func(string) string { return "" }, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run("--help"); code != 0 || !strings.Contains(out, "usage: robin-send FILE...") {
		t.Errorf("--help: %d %q", code, out)
	}
	if code, _, errOut := run("--", "x"); code != 1 || errOut != "robin-send: no robin session is running here\n" {
		t.Errorf("without a session: %d %q", code, errOut)
	}

	os.Mkdir(filepath.Join(root, "9"), 0o700)
	l, err := files.Listen(filepath.Join(root, "9", files.SocketName), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		l.Serve(func(r *files.Request) {
			defer r.Close()
			switch r.Files[0].Name {
			case "keep":
				r.Tell(0, files.Saved, "")
			case "wait":
				// As if the user pressed Ctrl+C with the file on offer.
				r.Tell(0, files.Offered, "")
				interrupt()
				<-r.Gone()
			default:
				r.Tell(0, files.Declined, "")
			}
		})
	}()
	t.Cleanup(func() {
		l.Close()
		<-served
	})
	file := func(name string) string {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0o600)
		return p
	}
	if code, out, _ := run(file("keep")); code != 0 || out != "keep: saved by the browser\n" {
		t.Errorf("a saved file: %d %q", code, out)
	}
	if code, out, errOut := run(file("drop")); code != 1 || errOut != "" || !strings.Contains(out, "declined") {
		t.Errorf("a declined file: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := run(file("wait")); code != 130 || !strings.Contains(errOut, "withdrawn") {
		t.Errorf("interrupted: %d %q", code, errOut)
	}
}
