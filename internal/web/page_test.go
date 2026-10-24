package web

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPageModules runs the unit tests of the page's DOM-free modules,
// static/test/*.test.js, in node.
func TestPageModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("the page's unit tests need node on PATH")
	}
	files, err := filepath.Glob(filepath.Join("static", "test", "*.test.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no page tests found: %v", err)
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
