# The remote host for the wayland scenario: the headless GNOME session of
# internal/agent/wayland/testdata (mutter, PipeWire with pipewire-pulse, the
# GNOME portal, a GTK probe window), reached over sshd as robin like the X11
# host. BASE is that directory's image; run.sh builds both and stages this
# build context (sshd_config, session.sh, probe.py, robin-wayland, robin-consent).
ARG BASE
FROM $BASE
USER root

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server \
 && rm -rf /var/lib/apt/lists/*

# The base image's session user is "ubuntu" at uid 1000; the tests log in as
# robin. A "*" password keeps the account unlocked for sshd without PAM while
# allowing no password login. The home is copied, not moved: rootless
# overlay storage cannot rename a directory from a lower layer.
RUN cp -a /home/ubuntu /home/robin && rm -rf /home/ubuntu \
 && usermod -l robin -d /home/robin -s /bin/bash ubuntu \
 && groupmod -n robin ubuntu \
 && usermod -p '*' robin \
 && mkdir -p /run/sshd /etc/ssh/robin /etc/ssh/authorized_keys.d /usr/local/lib/robin \
 && rm -f /etc/ssh/ssh_host_* /etc/ssh/sshd_config.d/*

COPY sshd_config /etc/ssh/sshd_config
COPY session.sh probe.py /usr/local/lib/robin/
COPY robin-wayland robin-consent /usr/local/bin/
RUN chmod 0755 /usr/local/lib/robin/session.sh /usr/local/bin/robin-wayland /usr/local/bin/robin-consent

# No logind or systemd: mutter falls back to its dummy login manager once
# /run/systemd/seats is gone, and wants a system bus of some kind. robin's
# session itself starts over ssh (robin-wayland start).
CMD ["/bin/sh", "-c", "rm -rf /run/systemd/seats && mkdir -p /run/sshd /run/dbus /run/user/1000 && chown robin:robin /run/user/1000 && chmod 700 /run/user/1000 && dbus-daemon --config-file=/etc/robin-system-bus.conf --fork && exec /usr/sbin/sshd -D -e"]
