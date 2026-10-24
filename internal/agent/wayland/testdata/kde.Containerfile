# KDE Plasma 6.7 for the Wayland tests: kwin_wayland with a virtual output
# and xdg-desktop-portal-kde on Fedora 43. See wltest.sh.
FROM docker.io/library/fedora:43
RUN dnf -y install --setopt=install_weak_deps=False \
    kwin-wayland xdg-desktop-portal xdg-desktop-portal-kde kf6-frameworkintegration breeze-icon-theme \
    at-spi2-core \
    pipewire pipewire-pulseaudio pipewire-utils pulseaudio-utils wireplumber pipewire-gstreamer \
    gstreamer1 gstreamer1-plugins-base gstreamer1-plugins-good gstreamer1-plugins-bad-free \
    dbus-daemon dbus-tools glib2 mesa-dri-drivers mesa-libEGL \
    python3-gobject python3-cairo gobject-introspection gtk4 \
    procps-ng psmisc \
 && dnf clean all \
 && useradd -m -u 1000 tester \
 # kwin_wayland carries a file capability (cap_sys_nice) that rootless podman
 # refuses to exec; a plain copy drops it.
 && cp /usr/bin/kwin_wayland /usr/local/bin/kwin_wayland
COPY system-bus.conf /etc/robin-system-bus.conf
ENV LANG=C.UTF-8
USER tester
