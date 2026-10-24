"""A fullscreen GTK 4 window that prints what it receives as JSON lines on
stdout, so a test can check injected input in surface coordinates, which
equal monitor coordinates here.

F2 prints the clipboard's text; F3 sets the clipboard to "from-session".
F4 prints the clipboard's MIME types and reads its text/html and image/png;
F5 sets the clipboard to RICH_TEXT, RICH_HTML and RICH_PNG at once.
Each key press, and each SIGUSR1, moves the picture on by one frame, the way
a terminal echoes what is typed. --animate repaints the whole window 60
times a second, for frame rate tests.
"""
import hashlib
import json
import signal
import sys
import time

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Gdk", "4.0")
from gi.repository import Gdk, GLib, Gtk  # noqa: E402

ANIMATE = "--animate" in sys.argv

RICH_TEXT = "rich-from-session"
RICH_HTML = "<b>rich</b> from-session"
# Bigger than a pipe's buffer, so the portal has to stream it. The clipboard
# does not look inside, so it need not decode.
RICH_PNG = b"\x89PNG\r\n\x1a\n" + bytes(range(256)) * 1024


def log(**kw):
    kw["t"] = round(time.monotonic(), 3)
    print(json.dumps(kw), flush=True)


class Probe(Gtk.Application):
    def __init__(self):
        super().__init__(application_id="dev.robin.Probe")
        self.frame = 0

    def do_activate(self):
        win = Gtk.ApplicationWindow(application=self, title="probe")
        win.set_decorated(False)
        area = Gtk.DrawingArea(hexpand=True, vexpand=True, focusable=True)
        area.set_draw_func(self.draw)
        win.set_child(area)

        motion = Gtk.EventControllerMotion()
        motion.connect("motion", lambda c, x, y: log(ev="motion", x=x, y=y))
        # A pointer arriving over the window reports an enter, not a motion.
        motion.connect("enter", lambda c, x, y: log(ev="motion", x=x, y=y))
        area.add_controller(motion)

        click = Gtk.GestureClick(button=0)
        click.connect("pressed", lambda c, n, x, y: log(ev="press", button=c.get_current_button(), x=x, y=y))
        click.connect("released", lambda c, n, x, y: log(ev="release", button=c.get_current_button(), x=x, y=y))
        area.add_controller(click)

        scroll = Gtk.EventControllerScroll(flags=Gtk.EventControllerScrollFlags.BOTH_AXES)
        scroll.connect("scroll", lambda c, dx, dy: log(ev="scroll", dx=dx, dy=dy) or True)
        area.add_controller(scroll)

        keys = Gtk.EventControllerKey()
        keys.connect("key-pressed", self.on_key)
        keys.connect("key-released", lambda c, kv, kc, st: log(ev="key-up", keyval=Gdk.keyval_name(kv)))
        win.add_controller(keys)

        self.clipboard = win.get_clipboard()
        self.area = area
        GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGUSR1, self.tick, area)
        if ANIMATE:
            # A fixed 60 Hz timer: a headless frame clock can run unthrottled.
            GLib.timeout_add(16, self.tick, area)
        win.fullscreen()
        win.present()
        GLib.timeout_add(500, lambda: log(ev="ready", w=area.get_width(), h=area.get_height()) and False)

    def tick(self, area):
        self.frame += 1
        area.queue_draw()
        return True

    def draw(self, area, cr, w, h):
        f = self.frame
        cr.set_source_rgb((f % 60) / 60, ((f // 2) % 60) / 60, 0.5)
        cr.paint()
        cr.set_source_rgb(1, 1, 1)
        cr.rectangle((f * 8) % max(w, 1), 0, 40, h)
        cr.fill()

    def on_key(self, ctrl, keyval, keycode, state):
        name = Gdk.keyval_name(keyval)
        log(ev="key", keyval=name, text=chr(Gdk.keyval_to_unicode(keyval) or 0))
        self.tick(self.area)
        if name == "F2":
            self.clipboard.read_text_async(None, self.on_text)
        elif name == "F3":
            self.clipboard.set("from-session")
        elif name == "F4":
            log(ev="formats", mimes=list(self.clipboard.get_formats().get_mime_types() or []))
            for mime in ("text/html", "image/png"):
                self.clipboard.read_async([mime], GLib.PRIORITY_DEFAULT, None, self.on_read, mime)
        elif name == "F5":
            parts = [(RICH_HTML.encode(), "text/html"), (RICH_PNG, "image/png"),
                     (RICH_TEXT.encode(), "text/plain;charset=utf-8")]
            self.clipboard.set_content(Gdk.ContentProvider.new_union(
                [Gdk.ContentProvider.new_for_bytes(mime, GLib.Bytes.new(data)) for data, mime in parts]))
        return True

    def on_text(self, cb, res):
        try:
            log(ev="clipboard", text=cb.read_text_finish(res))
        except GLib.Error as e:
            log(ev="clipboard-error", error=e.message)

    def on_read(self, cb, res, mime):
        try:
            stream, got = cb.read_finish(res)
            chunks = []
            while True:
                b = stream.read_bytes(65536, None)
                if b.get_size() == 0:
                    break
                chunks.append(b.get_data())
            data = b"".join(chunks)
            log(ev="read", mime=got, size=len(data), sha256=hashlib.sha256(data).hexdigest(),
                text=data.decode(errors="replace") if mime == "text/html" else "")
        except GLib.Error as e:
            log(ev="read-error", mime=mime, error=e.message)


Probe().run([sys.argv[0]])
