# The e2e host booted by systemd: the X11 host image with systemd as PID 1,
# sshd logging in through PAM so every login is a logind session, logind
# killing a login's processes when it ends, and robin lingering so its user
# manager runs from boot. A kept session survives here only as a unit of
# that manager.
# Build context: e2e/sdhost. BASE is the X11 host image run.sh builds.
ARG BASE
FROM ${BASE}

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus libpam-systemd \
 && rm -rf /var/lib/apt/lists/*

COPY kill-user-processes.conf /etc/systemd/logind.conf.d/robin-e2e.conf
# The socket unit would start sshd on demand; the service is plainer.
RUN sed -i 's/^UsePAM no$/UsePAM yes/' /etc/ssh/sshd_config \
 && grep -qx 'UsePAM yes' /etc/ssh/sshd_config \
 && install -d /var/lib/systemd/linger \
 && touch /var/lib/systemd/linger/robin \
 && systemctl disable ssh.socket \
 && systemctl enable ssh.service

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
