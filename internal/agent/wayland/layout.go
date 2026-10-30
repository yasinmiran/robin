//go:build linux

package wayland

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/xkb"
)

// GNOME keeps its keyboard layouts, input sources, under this schema, and
// dconf announces each change to them on the session bus.
const (
	inputSources = "org.gnome.desktop.input-sources"
	dconfPath    = dbus.ObjectPath("/ca/desrt/dconf/Writer/user")
	dconfWriter  = "ca.desrt.dconf.Writer"
	dconfPrefix  = "/org/gnome/desktop/input-sources/"
	// gsettingsTimeout bounds one read of a setting.
	gsettingsTimeout = 2 * time.Second
)

// keyboard follows the keyboard layout GNOME Shell gives mutter, which
// types a keysym only if that layout has it.
type keyboard struct {
	// source returns the input source in use, such as "ru" or "us+intl",
	// or "" when GNOME names none or one that is not an xkb layout.
	source func() (string, error)
	// load reads a layout's symbols.
	load func(layout, variant string) (xkb.Layout, error)
	log  *slog.Logger

	mu     sync.Mutex
	id     string
	layout xkb.Layout
	known  bool
	stop   func()
}

// followKeyboard reads the layout in use and follows its changes through
// dconf's announcements on sigs.
func followKeyboard(sigs *signals, log *slog.Logger) *keyboard {
	k := &keyboard{
		source: gnomeSource,
		load: func(layout, variant string) (xkb.Layout, error) {
			return xkb.Load(xkb.Dirs(), layout, variant)
		},
		log: log,
	}
	k.update()
	notes, stop, err := sigs.watch(dconfPath, dconfWriter, "Notify")
	if err != nil {
		log.Debug("keyboard layout changes cannot be followed", "err", err)
		return k
	}
	k.stop = stop
	go k.follow(notes)
	return k
}

// follow rereads the layout after each change dconf announces to the
// input sources.
func (k *keyboard) follow(notes <-chan *dbus.Signal) {
	for sig := range notes {
		if touchesInputSources(sig) {
			k.update()
		}
	}
}

// touchesInputSources reports whether a dconf Notify, (prefix, keys, tag),
// changed a key of the input sources schema.
func touchesInputSources(sig *dbus.Signal) bool {
	if len(sig.Body) < 2 {
		return false
	}
	prefix, _ := sig.Body[0].(string)
	keys, _ := sig.Body[1].([]string)
	if len(keys) == 0 {
		keys = []string{""}
	}
	for _, key := range keys {
		path := prefix + key
		if strings.HasPrefix(path, dconfPrefix) || strings.HasPrefix(dconfPrefix, path) {
			return true
		}
	}
	return false
}

// update reads the layout in use. One that cannot be read is not known,
// and then the keyboard takes it to have every ASCII character, as it did
// before it knew.
func (k *keyboard) update() {
	var layout xkb.Layout
	id, err := k.source()
	if err == nil && id != "" {
		layout, err = k.load(xkb.ParseSource(id))
	}
	known := err == nil && id != ""
	k.mu.Lock()
	changed := id != k.id || known != k.known
	k.id, k.layout, k.known = id, layout, known
	k.mu.Unlock()
	switch {
	case err != nil:
		k.log.Debug("the keyboard layout is not known", "source", id, "err", err)
	case changed && known:
		k.log.Debug("keyboard layout", "source", id, "name", layout.Name, "latin", layout.Latin())
	}
}

// lacks reports whether the layout in use is known not to type r, a
// printable ASCII character. Of the rest it knows nothing, and so reports
// false, as a nil keyboard does of everything.
func (k *keyboard) lacks(r rune) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.known && r > ' ' && r < 0x7f && !k.layout.Has(r)
}

// latinless reports whether the layout in use is known to type none of
// the letters a to z.
func (k *keyboard) latinless() bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.known {
		return false
	}
	for r := 'a'; r <= 'z'; r++ {
		if k.layout.Has(r) {
			return false
		}
	}
	return true
}

func (k *keyboard) close() {
	if k.stop != nil {
		k.stop()
	}
}

// gnomeSource returns the input source GNOME Shell applies: the most
// recently used of the configured ones, or the first.
func gnomeSource() (string, error) {
	sources, err := gsettings(inputSources, "sources")
	if err != nil {
		return "", err
	}
	mru, err := gsettings(inputSources, "mru-sources")
	if err != nil {
		return "", err
	}
	return currentSource(parseSources(sources), parseSources(mru)), nil
}

// currentSource picks the source in use from sources and the most recently
// used ones, as GNOME Shell does, and names it if it is an xkb layout.
func currentSource(sources, mru [][2]string) string {
	if len(sources) == 0 {
		return ""
	}
	current := sources[0]
	for _, m := range mru {
		if slices.Contains(sources, m) {
			current = m
			break
		}
	}
	if current[0] != "xkb" {
		return ""
	}
	return current[1]
}

// sourcePair matches one ('type', 'id') of a GVariant a(ss) as gsettings
// prints it.
var sourcePair = regexp.MustCompile(`\(\s*(?:'([^']*)'|"([^"]*)")\s*,\s*(?:'([^']*)'|"([^"]*)")\s*\)`)

// parseSources reads the a(ss) of input sources as gsettings prints it,
// such as [('xkb', 'us'), ('xkb', 'ru')] or @a(ss) [].
func parseSources(s string) [][2]string {
	var out [][2]string
	for _, m := range sourcePair.FindAllStringSubmatch(s, -1) {
		out = append(out, [2]string{m[1] + m[2], m[3] + m[4]})
	}
	return out
}

// gsettings reads a key of schema, as gsettings prints it.
func gsettings(schema, key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gsettingsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gsettings", "get", schema, key)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("gsettings get %s %s: %w: %s", schema, key, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("gsettings get %s %s: %w", schema, key, err)
	}
	return string(out), nil
}
