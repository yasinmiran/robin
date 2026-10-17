# The remote host for the wayland scenario: the headless GNOME session of
# internal/agent/wayland/testdata (mutter, PipeWire with pipewire-pulse, the
# GNOME portal, a GTK probe window), reached over sshd as rj like the X11
# host. BASE is that directory's image; run.sh builds both and stages this
# build context (sshd_config, session.sh, probe.py, rj-wayland, rj-consent).
ARG BASE
FROM $BASE
USER root

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server \
 && rm -rf /var/lib/apt/lists/*

# The base image's session user is "ubuntu" at uid 1000; the tests log in as
# rj. A "*" password keeps the account unlocked for sshd without PAM while
# allowing no password login. The home is copied, not moved: rootless
# overlay storage cannot rename a directory from a lower layer.
RUN cp -a /home/ubuntu /home/rj && rm -rf /home/ubuntu \
 && usermod -l rj -d /home/rj -s /bin/bash ubuntu \
 && groupmod -n rj ubuntu \
 && usermod -p '*' rj \
 && mkdir -p /run/sshd /etc/ssh/rj /etc/ssh/authorized_keys.d /usr/local/lib/rj \
 && rm -f /etc/ssh/ssh_host_* /etc/ssh/sshd_config.d/*

COPY sshd_config /etc/ssh/sshd_config
COPY session.sh probe.py /usr/local/lib/rj/
COPY rj-wayland rj-consent /usr/local/bin/
RUN chmod 0755 /usr/local/lib/rj/session.sh /usr/local/bin/rj-wayland /usr/local/bin/rj-consent

# No logind or systemd: mutter falls back to its dummy login manager once
# /run/systemd/seats is gone, and wants a system bus of some kind. rj's
# session itself starts over ssh (rj-wayland start).
CMD ["/bin/sh", "-c", "rm -rf /run/systemd/seats && mkdir -p /run/sshd /run/dbus /run/user/1000 && chown rj:rj /run/user/1000 && chmod 700 /run/user/1000 && dbus-daemon --config-file=/etc/rj-system-bus.conf --fork && exec /usr/sbin/sshd -D -e"]
