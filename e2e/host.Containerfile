# The remote host for remotejump e2e: sshd plus what spawn mode needs on a real
# host (Xvfb, a light session), plus the tools the tests use to inspect it.
# Build context: e2e/host.
FROM docker.io/library/ubuntu:24.04

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server \
      xvfb xauth xkb-data x11-xkb-utils xfonts-base \
      openbox xterm dbus-x11 \
      x11-utils x11-xserver-utils xdotool xclip \
      iptables sudo procps netcat-openbsd \
 && rm -rf /var/lib/apt/lists/* \
 && dbus-uuidgen > /etc/machine-id

# The base image ships user "ubuntu" at uid 1000. A "*" password keeps the
# account unlocked for sshd without PAM while allowing no password login.
RUN userdel -r ubuntu \
 && useradd -m -u 1000 -U -s /bin/bash rj \
 && usermod -p '*' rj \
 && mkdir -p /run/sshd /etc/ssh/rj /etc/ssh/authorized_keys.d \
 && install -d -m 1777 /tmp/.X11-unix \
 && rm -f /etc/ssh/ssh_host_* /etc/ssh/sshd_config.d/*

COPY sshd_config /etc/ssh/sshd_config
COPY sudoers-rj /etc/sudoers.d/rj
COPY rj-rundir rj-x rj-block-udp rj-live /usr/local/bin/
RUN chmod 0440 /etc/sudoers.d/rj \
 && chmod 0755 /usr/local/bin/rj-rundir /usr/local/bin/rj-x /usr/local/bin/rj-block-udp /usr/local/bin/rj-live \
 && visudo -cf /etc/sudoers.d/rj

EXPOSE 22
# /run/sshd is recreated in case the runtime mounts a fresh /run.
CMD ["/bin/sh", "-c", "mkdir -p /run/sshd && exec /usr/sbin/sshd -D -e"]
