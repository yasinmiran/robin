package web

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/yasinmiran/robin/internal/protocol"
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

// TestPageLimitsMatchTheAgent checks the limits the page restates from
// internal/protocol. A page that sent more than the agent takes would have
// what it sent refused, and the user would see nothing typed.
func TestPageLimitsMatchTheAgent(t *testing.T) {
	for _, c := range []struct {
		file, name string
		want       int
	}{
		{"ime.js", "MAX_TEXT_BYTES", protocol.MaxTextBytes},
		{"bulkframe.js", "CHUNK_SIZE", protocol.BulkChunkSize},
		{"tiles.js", "TILE_SIZE", protocol.TileSize},
		{"tiles.js", "MAX_TILE_BATCH", protocol.MaxTileBatch},
		{"tiles.js", "TILE_ATLAS_COLUMNS", protocol.TileAtlasColumns},
		{"tiles.js", "TILE_BATCH_LIMIT", protocol.TileBatchLimit},
		{"tiles.js", "MAX_TILE_GRID", protocol.MaxTileGrid},
		// What the page saves the CLI checks again, and refuses past these.
		{"macro.js", "MAX_MACROS", protocol.MaxMacros},
		{"macro.js", "MAX_MACRO_STEPS", protocol.MaxMacroSteps},
		{"macro.js", "MAX_MACRO_CHORD_KEYS", protocol.MaxMacroChordKeys},
		{"macro.js", "MAX_MACRO_NAME_BYTES", protocol.MaxMacroNameBytes},
		{"macro.js", "MAX_MACRO_TEXT_BYTES", protocol.MaxMacroTextBytes},
		{"macro.js", "MAX_MACRO_PAUSE_MS", protocol.MaxMacroPauseMs},
		{"macro.js", "MAX_MACRO_KEY_BYTES", protocol.MaxMacroKeyBytes},
		{"macro.js", "MAX_MACRO_HOST_BYTES", protocol.MaxMacroHostBytes},
		{"macro.js", "MAX_MACROS_BYTES", protocol.MaxMacrosBytes},
		{"macro.js", "MAX_MACRO_ID_BYTES", protocol.MaxMacroIDBytes},
	} {
		got, err := pageConst(filepath.Join("static", c.file), c.name)
		if err != nil {
			t.Error(err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: %s = %d, the agent's limit is %d", c.file, c.name, got, c.want)
		}
	}
}

// pageConst reads a module's "export const name = a;" or "= a * b;".
func pageConst(path, name string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	m := regexp.MustCompile(`(?m)^export const ` + name + ` = (\d+)(?: \* (\d+))?;`).FindSubmatch(b)
	if m == nil {
		return 0, fmt.Errorf("%s: no export const %s = <number>", path, name)
	}
	n, _ := strconv.Atoi(string(m[1]))
	if len(m[2]) > 0 {
		k, _ := strconv.Atoi(string(m[2]))
		n *= k
	}
	return n, nil
}
