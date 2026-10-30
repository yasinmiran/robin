# GNOME 49 for the Wayland tests: the same session as Containerfile (GNOME
# 46 on Ubuntu 24.04), on Fedora 43. See wltest.sh.
FROM docker.io/library/fedora:43
RUN dnf -y install --setopt=install_weak_deps=False \
    mutter xdg-desktop-portal xdg-desktop-portal-gnome \
    pipewire pipewire-pulseaudio pipewire-utils pulseaudio-utils wireplumber pipewire-gstreamer \
    gstreamer1 gstreamer1-plugins-base gstreamer1-plugins-good gstreamer1-plugins-bad-free \
    dbus-daemon dbus-tools glib2 mesa-dri-drivers mesa-libEGL \
    python3-gobject python3-cairo gtk4 gsettings-desktop-schemas dconf \
    procps-ng psmisc \
 && dnf clean all \
 && useradd -m -u 1000 tester
COPY system-bus.conf /etc/robin-system-bus.conf
ENV LANG=C.UTF-8
USER tester
