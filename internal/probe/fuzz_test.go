package probe

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FuzzParse feeds Parse what a host prints for the probe: the fixtures,
// gathered from real hosts and hostile ones, mutated. Whatever a host
// prints, Parse ends without a panic, what it reads does not depend on
// what came before the header, and every agent hash it keeps is
// lowercase.
func FuzzParse(f *testing.F) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "*.txt"))
	if err != nil || len(fixtures) == 0 {
		f.Fatalf("no fixtures: %v", err)
	}
	for _, p := range fixtures {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte("motd\r\nprobe=1\r\nuid=x\r\n"))
	f.Add([]byte("probe=1\nkept_session={\"id\":7}\nagent_sha256=a\nagentdir_rejected=/tmp\n"))
	f.Fuzz(func(t *testing.T, out []byte) {
		r, err := Parse(out)
		if err != nil {
			return
		}
		again, err := Parse(append([]byte("Last login: never\nprobe=0\n"), out...))
		if err != nil || !reflect.DeepEqual(again, r) {
			t.Fatalf("what came before the header changed the report: %v", err)
		}
		for name, sum := range r.agentHashes {
			if sum != strings.ToLower(sum) {
				t.Fatalf("the hash of %s is %q", name, sum)
			}
		}
		r.SpawnMissing()
		r.WaylandMissing()
		r.InstallHint()
		r.WaylandInstallHint()
		r.CameraDevice()
		r.AutoEncoder(1920, 1080)
		for _, e := range []string{"software", "vaapi", "nvenc", "auto", ""} {
			r.CheckEncoder(e)
		}
	})
}

// TestParseIsLinearInTheOutput: a host may print up to the megabyte
// sshmux.Output keeps, all of it values of keys that collect a list.
// Parse takes each in constant time, so the most a host can print parses
// at once rather than in quadratic time.
func TestParseIsLinearInTheOutput(t *testing.T) {
	for _, key := range []string{"xauth", "ffmpeg_encoder", "gst_plugin", "gst_dir"} {
		var b bytes.Buffer
		b.WriteString(header + "\n")
		for i := 0; b.Len() < 1<<20; i++ {
			fmt.Fprintf(&b, "%s=%d\n", key, i)
		}
		start := time.Now()
		r, err := Parse(b.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("a megabyte of %s took %s", key, took)
		}
		if n := len(r.XAuthority) + len(r.FFmpegEncoders) + len(r.GstPlugins) + len(r.GstDirs); n < 50000 {
			t.Errorf("a megabyte of %s gave %d values", key, n)
		}
	}
}
