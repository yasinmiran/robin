# The remote host for robin e2e: sshd plus what spawn mode needs on a real
# host (Xvfb, a light session, pulseaudio for sound), plus the tools the tests
# use to inspect it.
# Build context: e2e/host.
FROM docker.io/library/ubuntu:24.04

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server \
      xvfb xauth xkb-data x11-xkb-utils xfonts-base \
      openbox xterm dbus-x11 \
      pulseaudio pulseaudio-utils \
      x11-utils x11-xserver-utils xdotool xclip \
      iptables sudo procps netcat-openbsd \
 && rm -rf /var/lib/apt/lists/* \
 && dbus-uuidgen > /etc/machine-id

# xwd: the screen as X has it, which sharp text's tiles must match exactly.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends x11-apps \
 && rm -rf /var/lib/apt/lists/*

# The base image ships user "ubuntu" at uid 1000. A "*" password keeps the
# account unlocked for sshd without PAM while allowing no password login.
RUN userdel -r ubuntu \
 && useradd -m -u 1000 -U -s /bin/bash robin \
 && usermod -p '*' robin \
 && mkdir -p /run/sshd /etc/ssh/robin /etc/ssh/authorized_keys.d \
 && install -d -m 1777 /tmp/.X11-unix /cover \
 && install -d -o robin -g robin -m 0700 /run/user/1000 \
 && rm -f /etc/ssh/ssh_host_* /etc/ssh/sshd_config.d/*

COPY sshd_config /etc/ssh/sshd_config
COPY sudoers-robin /etc/sudoers.d/robin
COPY robin-rundir robin-x robin-block-udp robin-live robin-room /usr/local/bin/
RUN chmod 0440 /etc/sudoers.d/robin \
 && chmod 0755 /usr/local/bin/robin-rundir /usr/local/bin/robin-x /usr/local/bin/robin-block-udp /usr/local/bin/robin-live /usr/local/bin/robin-room \
 && visudo -cf /etc/sudoers.d/robin

EXPOSE 22
# /run/sshd is recreated in case the runtime mounts a fresh /run.
CMD ["/bin/sh", "-c", "mkdir -p /run/sshd && exec /usr/sbin/sshd -D -e"]
