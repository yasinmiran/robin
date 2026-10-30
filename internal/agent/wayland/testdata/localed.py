"""A stand-in for systemd-localed on the system bus, which has none in the
container: it says the system's keyboard layout is LAYOUT, which mutter's
default plugin, the compositor of a session without GNOME Shell, applies
as it starts. Usage: localed.py LAYOUT
"""
import sys

from gi.repository import Gio, GLib

XML = """<node><interface name="org.freedesktop.locale1">
<property name="X11Layout" type="s" access="read"/>
<property name="X11Model" type="s" access="read"/>
<property name="X11Variant" type="s" access="read"/>
<property name="X11Options" type="s" access="read"/>
<property name="Locale" type="as" access="read"/>
</interface></node>"""

PROPS = {
    "X11Layout": GLib.Variant("s", sys.argv[1]),
    "X11Model": GLib.Variant("s", ""),
    "X11Variant": GLib.Variant("s", ""),
    "X11Options": GLib.Variant("s", ""),
    "Locale": GLib.Variant("as", ["LANG=C.UTF-8"]),
}


def get_property(conn, sender, path, iface, name):
    return PROPS[name]


def on_bus(conn, name):
    info = Gio.DBusNodeInfo.new_for_xml(XML).interfaces[0]
    conn.register_object("/org/freedesktop/locale1", info, None, get_property, None)


def on_name(conn, name):
    print("ready", flush=True)


Gio.bus_own_name(Gio.BusType.SYSTEM, "org.freedesktop.locale1", Gio.BusNameOwnerFlags.NONE, on_bus, on_name, None)
GLib.MainLoop().run()
