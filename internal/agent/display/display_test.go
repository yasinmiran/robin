//go:build linux

package display

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary act as an agent that spawns a display and
// waits to be killed; see TestAgentDeathKillsEverything. With
// helperCloseEnv set too, it starts closing the display once a line comes
// on stdin and kills itself while Close waits for the session's groups.
const (
	helperEnv      = "ROBIN_DISPLAY_TEST_HELPER"
	helperCloseEnv = "ROBIN_DISPLAY_TEST_DIE_IN_CLOSE"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(helperEnv); dir != "" {
		d, err := Spawn(context.Background(), Options{RunDir: dir, MaxWidth: 640, MaxHeight: 480, Session: treeSession})
		if err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("ready " + d.Name + "\n")
		if os.Getenv(helperCloseEnv) != "" {
			bufio.NewReader(os.Stdin).ReadString('\n')
			sleeper := findProcs(func(_ int, cmdline, environ string) bool {
				return cmdline == "sleep\x001000\x00" && strings.Contains("\x00"+environ, "\x00XAUTHORITY="+d.XAuthority+"\x00")
			})
			if len(sleeper) == 0 {
				os.Stdout.WriteString("no sleep 1000 in the session\n")
				select {}
			}
			go d.Close()
			// SIGTERM takes the sleep; Close then waits on the one that
			// ignores it.
			for len(living(sleeper)) > 0 {
				time.Sleep(5 * time.Millisecond)
			}
			os.Stdout.WriteString("closing\n")
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		select {}
	}
	os.Exit(m.Run())
}

// treeSession forks an X client, a plain grandchild and one that ignores
// SIGTERM, so teardown has to reach past the direct child.
const treeSession = `xterm & sleep 1000 & sh -c 'trap "" TERM; while :; do sleep 1; done' & wait`

func needXvfb(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not on PATH; run internal/agent/x11/testdata/xtest.sh")
	}
}

func spawn(t *testing.T, session string) (*Display, string) {
	t.Helper()
	needXvfb(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Spawn(ctx, Options{RunDir: dir, MaxWidth: 800, MaxHeight: 600, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dir
}

func TestXauthorityFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Xauthority")
	cookie := bytes.Repeat([]byte{0xab}, 16)
	if err := writeXauthority(path, cookie, "42"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	entries := readEntries(t, path)
	host, _ := os.Hostname()
	want := []entry{{familyLocal, host, "42"}, {familyWild, "", "42"}}
	if len(entries) != len(want) {
		t.Fatalf("entries %+v, want %+v", entries, want)
	}
	for i, e := range entries {
		if e.entry != want[i] || e.name != "MIT-MAGIC-COOKIE-1" || !bytes.Equal(e.data, cookie) {
			t.Errorf("entry %d = %+v", i, e)
		}
	}

	if err := writeXauthority(path, cookie, ""); err != nil {
		t.Fatal(err)
	}
	if e := readEntries(t, path); len(e) != 1 || e[0].entry != (entry{familyWild, "", ""}) {
		t.Errorf("pre-start file %+v, want one wildcard entry", e)
	}
}

type entry struct {
	family uint16
	addr   string
	num    string
}

type fullEntry struct {
	entry
	name string
	data []byte
}

func readEntries(t *testing.T, path string) []fullEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(b)
	var out []fullEntry
	for r.Len() > 0 {
		var e fullEntry
		binary.Read(r, binary.BigEndian, &e.family)
		var f [4][]byte
		for i := range f {
			var n uint16
			if err := binary.Read(r, binary.BigEndian, &n); err != nil {
				t.Fatal("truncated")
			}
			f[i] = make([]byte, n)
			io.ReadFull(r, f[i])
		}
		e.addr, e.num, e.name, e.data = string(f[0]), string(f[1]), string(f[2]), f[3]
		out = append(out, e)
	}
	return out
}

func TestPickSession(t *testing.T) {
	for _, tc := range []struct {
		have   string
		custom string
		name   string
		main   string
		comp   string
	}{
		{"xfce4-session dbus-run-session openbox xterm", "", "xfce4-session", "/bin/dbus-run-session -- /bin/xfce4-session", ""},
		{"xfce4-session", "", "xfce4-session", "/bin/xfce4-session", ""},
		{"openbox openbox-session xterm xfce4-terminal", "", "openbox-session + xterm", "/bin/openbox-session", "/bin/xterm"},
		{"twm xfce4-terminal", "", "twm + xfce4-terminal", "/bin/twm", "/bin/xfce4-terminal"},
		{"fluxbox", "", "fluxbox", "/bin/fluxbox", ""},
		{"x-terminal-emulator", "", "x-terminal-emulator", "/bin/x-terminal-emulator", ""},
		{"", "my-session --flag", "my-session --flag", "/bin/sh -c my-session --flag", ""},
	} {
		have := strings.Fields(tc.have)
		look := func(name string) (string, error) {
			for _, h := range have {
				if h == name {
					return "/bin/" + name, nil
				}
			}
			return "", exec.ErrNotFound
		}
		p, err := pickSession(tc.custom, look)
		if err != nil {
			t.Errorf("%q: %v", tc.have, err)
			continue
		}
		if p.name != tc.name || strings.Join(p.main, " ") != tc.main || strings.Join(p.companion, " ") != tc.comp {
			t.Errorf("%q: got %q main %q companion %q", tc.have, p.name, p.main, p.companion)
		}
	}
	if _, err := pickSession("", func(string) (string, error) { return "", exec.ErrNotFound }); err == nil {
		t.Error("no session installed: want an error")
	}
}

func TestSpawn(t *testing.T) {
	d, dir := spawn(t, "sleep 1000")
	if !strings.HasPrefix(d.Name, ":") || d.XAuthority != filepath.Join(dir, "Xauthority") {
		t.Fatalf("display %+v", d)
	}
	b, err := os.ReadFile(filepath.Join(dir, "display"))
	if err != nil || string(b) != d.Name+"\n" {
		t.Fatalf("display file %q, %v; want %q", b, err, d.Name+"\n")
	}
	for _, f := range []string{"display", "Xauthority"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}

	if _, err := exec.LookPath("xdpyinfo"); err == nil {
		// libXau must accept our file, and the server must refuse without it.
		run := func(xauth string) error {
			cmd := exec.Command("xdpyinfo", "-display", d.Name)
			cmd.Env = append(os.Environ(), "XAUTHORITY="+xauth)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return errors.New(strings.TrimSpace(string(out)))
			}
			return nil
		}
		if err := run(d.XAuthority); err != nil {
			t.Errorf("xdpyinfo with our Xauthority: %v", err)
		}
		if err := run("/dev/null"); err == nil {
			t.Error("xdpyinfo without a cookie connected; the server is not enforcing auth")
		}
	}

	select {
	case err := <-d.Done():
		t.Fatalf("Done fired early: %v", err)
	default:
	}
}

// TestAutoSession runs the default pick for the test image: a window manager
// with a terminal in the same process group.
func TestAutoSession(t *testing.T) {
	needXvfb(t)
	if _, err := exec.LookPath("openbox-session"); err != nil {
		t.Skip("no openbox-session")
	}
	d, _ := spawn(t, "")
	if d.Session != "openbox-session + xterm" {
		t.Fatalf("Session = %q", d.Session)
	}
	var pids []int
	var wm, term int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && (wm == 0 || term == 0); time.Sleep(100 * time.Millisecond) {
		pids = findProcs(func(_ int, _, environ string) bool {
			return strings.Contains("\x00"+environ, "\x00XAUTHORITY="+d.XAuthority+"\x00")
		})
		for _, p := range pids {
			switch c := cmdline(p); {
			case strings.Contains(c, "openbox"):
				wm = p
			case strings.HasPrefix(c, "xterm") || strings.HasPrefix(c, "/usr/bin/xterm"):
				term = p
			}
		}
	}
	if wm == 0 || term == 0 {
		t.Fatalf("window manager or terminal missing: %v", describe(pids))
	}
	if gw, gt := pgid(wm), pgid(term); gw != gt {
		t.Errorf("terminal in group %d, window manager in %d", gt, gw)
	}
	// The terminal must be on screen promptly: started in openbox's startup
	// window, it used to stay unmapped for 12 s or more.
	if _, err := exec.LookPath("xdotool"); err == nil {
		visible := false
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !visible; time.Sleep(50 * time.Millisecond) {
			cmd := exec.Command("xdotool", "search", "--onlyvisible", "--class", "xterm")
			cmd.Env = append(os.Environ(), "DISPLAY="+d.Name, "XAUTHORITY="+d.XAuthority)
			visible = cmd.Run() == nil
		}
		if !visible {
			t.Error("terminal not visible within 2 s of Spawn")
		}
	}
	select {
	case err := <-d.Done():
		t.Fatalf("Done fired: %v", err)
	default:
	}
	d.Close()
	if alive := living(pids); len(alive) > 0 {
		t.Errorf("still alive after Close: %v", describe(alive))
	}
}

func pgid(pid int) int {
	g, _ := syscall.Getpgid(pid)
	return g
}

func TestSessionExitFiresDone(t *testing.T) {
	d, _ := spawn(t, "echo bye >&2; exit 3")
	select {
	case err := <-d.Done():
		if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "bye") {
			t.Errorf("Done = %v, want exit status 3 with stderr", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire")
	}
	if err, ok := <-d.Done(); ok || err != nil {
		t.Errorf("second receive = %v, %v; want closed", err, ok)
	}
}

func TestXvfbFailure(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "Xvfb")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'no screens found' >&2\nexit 1\n"), 0o755)
	dir := t.TempDir()
	_, err := Spawn(context.Background(), Options{RunDir: dir, MaxWidth: 640, MaxHeight: 480, Session: "true", Xvfb: fake})
	if err == nil || !strings.Contains(err.Error(), "no screens found") {
		t.Fatalf("Spawn = %v, want Xvfb's stderr in the error", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("RunDir not cleaned: %v", left)
	}
}

func TestCloseKillsEverything(t *testing.T) {
	d, dir := spawn(t, treeSession)
	pids := waitTree(t, dir, d.XAuthority)
	start := time.Now()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("Close took %v for %d processes", time.Since(start).Round(time.Millisecond), len(pids))
	if alive := living(pids); len(alive) > 0 {
		t.Errorf("still alive after Close: %v", describe(alive))
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("RunDir not cleaned: %v", left)
	}
	if watchdogs := findProcs(func(_ int, cmdline, _ string) bool {
		return strings.Contains(cmdline, "robin-display-watchdog")
	}); len(watchdogs) > 0 {
		t.Errorf("watchdog survived Close: %v", describe(watchdogs))
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestAgentDeathKillsEverything(t *testing.T) {
	needXvfb(t)
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), helperEnv+"="+dir)
	stdout, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if !strings.HasPrefix(line, "ready ") {
		helper.Process.Kill()
		t.Fatalf("helper: %q", line)
	}
	pids := waitTree(t, dir, filepath.Join(dir, "Xauthority"))

	helper.Process.Signal(syscall.SIGKILL)
	helper.Wait()
	deadline := time.Now().Add(6 * time.Second)
	var alive []int
	for time.Now().Before(deadline) {
		if alive = living(pids); len(alive) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(alive) > 0 {
		t.Errorf("outlived the agent: %v", describe(alive))
	}
	// The watchdog exits right after its last kill.
	watchdog := func() []int {
		return findProcs(func(_ int, cmdline, _ string) bool {
			return strings.Contains(cmdline, "robin-display-watchdog")
		})
	}
	w := watchdog()
	for deadline := time.Now().Add(3 * time.Second); len(w) > 0 && time.Now().Before(deadline); w = watchdog() {
		time.Sleep(50 * time.Millisecond)
	}
	if len(w) > 0 {
		t.Errorf("watchdog still running: %v", describe(w))
	}
}

// TestAgentDeathDuringCloseKillsEverything kills the agent while Close
// waits for the session's groups to go after SIGTERM: whatever ignored it
// still has to die with the agent, which only the watchdog can see to.
func TestAgentDeathDuringCloseKillsEverything(t *testing.T) {
	needXvfb(t)
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), helperEnv+"="+dir, helperCloseEnv+"=1")
	stdin, _ := helper.StdinPipe()
	stdout, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(stdout)
	line, _ := lines.ReadString('\n')
	if !strings.HasPrefix(line, "ready ") {
		helper.Process.Kill()
		t.Fatalf("helper: %q", line)
	}
	pids := waitTree(t, dir, filepath.Join(dir, "Xauthority"))

	stdin.Write([]byte("close\n"))
	if line, _ := lines.ReadString('\n'); line != "closing\n" {
		helper.Process.Kill()
		helper.Wait()
		for _, p := range living(pids) {
			syscall.Kill(p, syscall.SIGKILL)
		}
		t.Fatalf("helper: %q, want it killed while Close waits", line)
	}
	helper.Wait()
	deadline := time.Now().Add(6 * time.Second)
	var alive []int
	for time.Now().Before(deadline) {
		if alive = living(pids); len(alive) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(alive) > 0 {
		t.Errorf("outlived an agent killed in Close: %v", describe(alive))
		for _, p := range alive {
			syscall.Kill(p, syscall.SIGKILL)
		}
	}
}

// waitTree waits until the session tree is fully up and returns its pids:
// Xvfb (found by its -auth argument) and every process with our XAUTHORITY.
func waitTree(t *testing.T, dir, xauth string) []int {
	t.Helper()
	match := func(_ int, cmdline, environ string) bool {
		return strings.Contains(cmdline, "-auth\x00"+xauth) ||
			strings.Contains("\x00"+environ, "\x00XAUTHORITY="+xauth+"\x00")
	}
	var pids []int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		pids = findProcs(match)
		var xterm, ignorer bool
		for _, p := range pids {
			c := cmdline(p)
			xterm = xterm || strings.HasPrefix(c, "xterm")
			ignorer = ignorer || strings.Contains(c, "trap")
		}
		if xterm && ignorer && len(pids) >= 5 {
			return pids
		}
	}
	t.Fatalf("session tree did not come up: %v", describe(pids))
	return nil
}

func findProcs(match func(pid int, cmdline, environ string) bool) []int {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() || !running(pid) {
			continue
		}
		c, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		env, _ := os.ReadFile("/proc/" + e.Name() + "/environ")
		if match(pid, string(c), string(env)) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// running reports a live process; a zombie has already been killed.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, rest, _ := bytes.Cut(b, []byte(") "))
	return len(rest) > 0 && rest[0] != 'Z' && rest[0] != 'X'
}

func living(pids []int) []int {
	var alive []int
	for _, p := range pids {
		if running(p) {
			alive = append(alive, p)
		}
	}
	return alive
}

func cmdline(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " ")
}

func describe(pids []int) []string {
	var s []string
	for _, p := range pids {
		s = append(s, strconv.Itoa(p)+" "+cmdline(p))
	}
	return s
}
