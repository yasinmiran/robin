#!/usr/bin/env bash
# Runs the remotejump e2e suite in rootless podman.
#
#   ./run.sh                    every scenario: direct blocked restart
#   ./run.sh direct restart     just those
#   ./run.sh selftest           prove the harness itself, no CLI needed
#
# RJ_BIN overrides the CLI binary (default bin/remotejump-linux-amd64, relative
# to the repo root). RJ_RESTART_TIMEOUT_MS and RJ_SIGINT_PID_ONLY pass through
# to the tests. Artifacts land in e2e/out/<scenario>/.
set -euo pipefail

E2E=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(dirname "$E2E")
OUT=$E2E/out
KEYS=$OUT/keys

NET=rj-e2e
SUBNET=${RJ_E2E_SUBNET:-10.89.42.0/24}
HOST_IP=${RJ_E2E_HOST_IP:-10.89.42.10}
HOST_CTR=rj-e2e-host
CLIENT_CTR=rj-e2e-client
HOST_IMG=localhost/rj-e2e-host
CLIENT_IMG=localhost/rj-e2e-client

RJ_BIN=${RJ_BIN:-bin/remotejump-linux-amd64}
[[ $RJ_BIN = /* ]] || RJ_BIN=$ROOT/$RJ_BIN

ALL_SCENARIOS=(direct blocked restart)

log() { printf '\033[1m[e2e]\033[0m %s\n' "$*" >&2; }
die() { log "error: $*"; exit 2; }

remove_containers() {
  podman rm -f "$CLIENT_CTR" "$HOST_CTR" >/dev/null 2>&1 || true
}

cleanup() {
  remove_containers
  podman network rm "$NET" >/dev/null 2>&1 || true
}

build_images() {
  log "building $HOST_IMG"
  podman build -q -t "$HOST_IMG" -f "$E2E/host.Containerfile" "$E2E/host" >/dev/null
  log "building $CLIENT_IMG"
  podman build -q -t "$CLIENT_IMG" -f "$E2E/client.Containerfile" "$E2E" >/dev/null
}

create_network() {
  podman network rm "$NET" >/dev/null 2>&1 || true
  podman network create --subnet "$SUBNET" "$NET" >/dev/null
}

generate_keys() {
  rm -rf "$KEYS"
  mkdir -p "$KEYS"
  ssh-keygen -q -t ed25519 -N '' -C rj-e2e-client -f "$KEYS/client"
  ssh-keygen -q -t ed25519 -N '' -C rj-e2e-host -f "$KEYS/ssh_host_ed25519_key"
  # HostKeyAlias keys known_hosts by name, so a fresh host IP never matters.
  echo "rjhost $(cut -d' ' -f1,2 "$KEYS/ssh_host_ed25519_key.pub")" >"$KEYS/known_hosts"
  cat >"$KEYS/ssh_config" <<EOF
Host rjhost
  HostName $HOST_IP
  HostKeyAlias rjhost
  User rj
  IdentityFile /keys/client
  IdentitiesOnly yes
  UserKnownHostsFile /keys/known_hosts
  StrictHostKeyChecking yes
  BatchMode yes
  ConnectTimeout 10
EOF
  chmod 600 "$KEYS"/*
  # sshd reads authorized keys as the user, and the mount is root-owned.
  chmod 644 "$KEYS/client.pub"
}

start_host() {
  podman run -d --name "$HOST_CTR" --hostname rjhost \
    --network "$NET" --ip "$HOST_IP" \
    --init --cap-add NET_ADMIN --shm-size 256m --stop-timeout 2 \
    -v "$KEYS/ssh_host_ed25519_key:/etc/ssh/rj/ssh_host_ed25519_key:ro" \
    -v "$KEYS/client.pub:/etc/ssh/authorized_keys.d/rj:ro" \
    "$HOST_IMG" >/dev/null
}

# start_client <artifact dir> [extra podman run args...]
start_client() {
  local dir=$1
  shift
  podman run -d --init --name "$CLIENT_CTR" \
    --network "$NET" --shm-size 1g --stop-timeout 2 \
    -v "$KEYS:/keys:ro" \
    -v "$KEYS/ssh_config:/root/.ssh/config:ro" \
    -v "$E2E/tests:/work/tests:ro" \
    -v "$E2E/playwright.config.ts:/work/playwright.config.ts:ro" \
    -v "$dir:/work/out" \
    "$@" "$CLIENT_IMG" sleep infinity >/dev/null
}

on_client() { podman exec -i "$CLIENT_CTR" "$@"; }
on_host_as_rj() { podman exec -i --user rj "$HOST_CTR" "$@"; }
client_ip() { podman inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$CLIENT_CTR"; }

wait_for_sshd() {
  local deadline=$((SECONDS + 30))
  until on_client ssh -o ConnectTimeout=2 rjhost true 2>/dev/null; do
    ((SECONDS < deadline)) || { podman logs "$HOST_CTR" 2>&1 | tail -n 20 >&2 || true; die "sshd on $HOST_IP never accepted the key"; }
    sleep 0.5
  done
}

save_host_state() {
  local dir=$1
  podman logs "$HOST_CTR" >"$dir/host-sshd.log" 2>&1 || true
  podman exec "$HOST_CTR" sh -c '
    echo "## processes"; ps -eo user,pid,ppid,etime,args
    echo "## iptables"; iptables -S; ip6tables -S
    echo "## remotejump dirs"; find /home/rj/.cache/remotejump /tmp/remotejump-1000 /run/user/1000 -maxdepth 4 -ls 2>/dev/null
  ' >"$dir/host-state.txt" 2>&1 || true
}

run_scenario() {
  local s=$1 dir=$OUT/$1 status=0
  rm -rf "$dir"
  mkdir -p "$dir"
  log "scenario $s: fresh host"
  remove_containers
  start_host
  start_client "$dir" -v "$RJ_BIN:/usr/local/bin/remotejump:ro"
  wait_for_sshd
  if [[ $s = blocked ]]; then
    log "scenario $s: dropping non-loopback UDP on the host"
    on_client ssh rjhost rj-block-udp
  fi
  on_client env SCENARIO="$s" \
    RJ_RESTART_TIMEOUT_MS="${RJ_RESTART_TIMEOUT_MS:-30000}" RJ_SIGINT_PID_ONLY="${RJ_SIGINT_PID_ONLY:-0}" \
    npx playwright test tests/gui.spec.ts || status=$?
  save_host_state "$dir"
  remove_containers
  return "$status"
}

# ---- selftest ---------------------------------------------------------------

SELFTEST_FAILED=0

check() {
  local name=$1
  shift
  if out=$("$@" 2>&1); then
    printf '  \033[32mPASS\033[0m %s\n' "$name"
  else
    printf '  \033[31mFAIL\033[0m %s\n' "$name"
    sed 's/^/       | /' <<<"$out"
    SELFTEST_FAILED=1
  fi
}

expect_eq() {
  local want=$1 got
  shift
  got=$("$@") || { echo "command failed: $*"; return 1; }
  [[ $got = "$want" ]] || { printf 'want %q\ngot  %q\n' "$want" "$got"; return 1; }
}

# retry: the one-shot listeners these checks start in the background may not
# be up yet on the first attempt.
retry() {
  local i
  for i in 1 2 3 4 5; do
    "$@" && return 0
    sleep 0.3
  done
  return 1
}

expect_fail() {
  if "$@"; then echo "unexpectedly succeeded: $*"; return 1; fi
}

t_ssh_login() { expect_eq $'rj 1000' on_client ssh rjhost 'echo "$(id -un) $(id -u)"'; }
t_no_root() { expect_fail on_client ssh -o User=root rjhost true; }
t_no_password() {
  # Only publickey may be offered; no password or keyboard-interactive prompt.
  local methods
  methods=$(on_client ssh -v -o IdentityFile=/dev/null -o PreferredAuthentications=none rjhost true 2>&1 \
    | tr -d '\r' | sed -n 's/.*Authentications that can continue: //p' | head -n1)
  [[ $methods = publickey ]] || { echo "server offers: ${methods:-nothing}"; return 1; }
}

t_sftp() {
  local token=sftp-$RANDOM$RANDOM
  on_client sh -c "printf $token >/tmp/sftp-up" &&
    on_client sftp -q -b - rjhost <<<$'put /tmp/sftp-up sftp-up\nget sftp-up /tmp/sftp-back' >/dev/null &&
    expect_eq "$token" on_client cat /tmp/sftp-back &&
    expect_eq "$token" on_client ssh rjhost cat sftp-up
}

M=/tmp/rj-master.sock
t_master() {
  on_client ssh -M -S "$M" -o ControlPersist=no -fN rjhost &&
    on_client ssh -S "$M" -O check rjhost 2>/dev/null &&
    expect_eq mux on_client ssh -S "$M" rjhost echo mux
}

t_forward_unix_to_tcp() {
  on_host_as_rj sh -c 'printf tok-tcp | nc -N -l 127.0.0.1 9999 >/dev/null 2>&1 &' &&
    on_client rm -f /tmp/x.sock &&
    on_client ssh -S "$M" -O forward -L /tmp/x.sock:127.0.0.1:9999 rjhost &&
    retry expect_eq tok-tcp on_client timeout 5 nc -N -U /tmp/x.sock </dev/null
}

t_forward_tcp_to_unix() {
  on_host_as_rj sh -c 'rm -f /tmp/rj-e2e.sock; printf tok-unix | nc -N -lU /tmp/rj-e2e.sock >/dev/null 2>&1 &' &&
    on_client ssh -S "$M" -O forward -L 127.0.0.1:19999:/tmp/rj-e2e.sock rjhost &&
    retry expect_eq tok-unix on_client timeout 5 nc -N 127.0.0.1 19999 </dev/null
}

# Chrome's active ICE-TCP candidate binds the interface address and connects to
# the passive candidate on 127.0.0.1, so the forward must accept that source.
t_forward_from_interface_addr() {
  on_host_as_rj sh -c 'rm -f /tmp/rj-e2e.sock; printf tok-bound | nc -N -lU /tmp/rj-e2e.sock >/dev/null 2>&1 &' &&
    retry expect_eq tok-bound on_client timeout 5 nc -N -s "$(client_ip)" 127.0.0.1 19999 </dev/null
}

t_master_exit() { on_client ssh -S "$M" -O exit rjhost 2>/dev/null; }

# udp_delivered <listener: client|host> <ip to send to> <port>: whether one
# datagram sent from the other container (or from the host itself when ip is
# 127.0.0.1) reaches a listener.
udp_delivered() {
  local where=$1 ip=$2 port=$3 token=udp-$RANDOM listen send
  if [[ $where = host ]]; then listen=on_host_as_rj; send=on_client; else listen=on_client; send=on_host_as_rj; fi
  [[ $ip = 127.0.0.1 ]] && send=$listen
  $listen sh -c "rm -f /tmp/udp-$port; timeout 4 nc -u -l $ip $port >/tmp/udp-$port 2>/dev/null &"
  local i
  for i in 1 2 3 4 5 6; do
    sleep 0.3
    $send sh -c "printf $token | timeout 2 nc -u -w1 $ip $port" 2>/dev/null || true
    if [[ $($listen cat /tmp/udp-$port 2>/dev/null) = *"$token"* ]]; then return 0; fi
  done
  return 1
}

t_udp_open_in() { udp_delivered host "$HOST_IP" 5000 || { echo "no datagram reached the host"; return 1; }; }
t_udp_open_out() { udp_delivered client "$(client_ip)" 5001 || { echo "no datagram reached the client"; return 1; }; }
t_block_over_ssh() {
  on_client ssh rjhost rj-block-udp || return 1
  local ipt rules
  for ipt in iptables ip6tables; do
    rules=$(on_client ssh rjhost sudo -n $ipt -w -S) || return 1
    grep -qx -- '-A INPUT ! -i lo -p udp -j DROP' <<<"$rules" &&
      grep -qx -- '-A OUTPUT ! -o lo -p udp -j DROP' <<<"$rules" ||
      { echo "$ipt rules:"; echo "$rules"; return 1; }
  done
}
t_sudo_is_narrow() { expect_fail on_client ssh rjhost sudo -n true; }
t_udp_blocked_in() { expect_fail udp_delivered host "$HOST_IP" 5002; }
t_udp_blocked_out() { expect_fail udp_delivered client "$(client_ip)" 5003; }
t_udp_loopback() { udp_delivered host 127.0.0.1 5004 || { echo "loopback UDP on the host is blocked"; return 1; }; }
t_ssh_after_block() { expect_eq ok on_client ssh rjhost echo ok; }

selftest() {
  local dir=$OUT/selftest status=0
  rm -rf "$dir"
  mkdir -p "$dir"
  remove_containers
  start_host
  start_client "$dir"
  wait_for_sshd

  log "selftest: ssh, sftp, forwarding"
  check "ssh: key login as rj (uid 1000)" t_ssh_login
  check "ssh: root login refused" t_no_root
  check "ssh: only publickey offered" t_no_password
  check "sftp: put and get round-trip" t_sftp
  check "ssh: ControlMaster up, exec rides it" t_master
  check "ssh -O forward: client unix socket -> host 127.0.0.1:9999" t_forward_unix_to_tcp
  check "ssh -O forward: client 127.0.0.1:19999 -> host unix socket" t_forward_tcp_to_unix
  check "forward accepts a socket bound to the client's interface address" t_forward_from_interface_addr
  check "ssh -O exit closes the master" t_master_exit

  log "selftest: UDP blocking"
  check "udp client -> host delivered before blocking" t_udp_open_in
  check "udp host -> client delivered before blocking" t_udp_open_out
  check "rj blocks UDP over ssh with sudo iptables" t_block_over_ssh
  check "rj's sudo allows nothing but iptables" t_sudo_is_narrow
  check "udp client -> host dropped" t_udp_blocked_in
  check "udp host -> client dropped" t_udp_blocked_out
  check "udp on host loopback still works" t_udp_loopback
  check "ssh still works with UDP blocked" t_ssh_after_block

  log "selftest: Chrome and host helpers (Playwright)"
  # A fresh host: the X helpers must not run with UDP blocked by the checks above.
  remove_containers
  start_host
  start_client "$dir"
  wait_for_sshd
  on_client env SCENARIO=selftest npx playwright test tests/selftest.spec.ts || status=$?
  save_host_state "$dir"
  remove_containers

  ((SELFTEST_FAILED == 0 && status == 0))
}

# ---- main -------------------------------------------------------------------

main() {
  local scenarios=("$@")
  ((${#scenarios[@]})) || scenarios=("${ALL_SCENARIOS[@]}")
  local s
  for s in "${scenarios[@]}"; do
    case $s in
      selftest | direct | blocked | restart) ;;
      *) die "unknown scenario '$s' (want selftest, direct, blocked or restart)" ;;
    esac
  done
  command -v podman >/dev/null || die "podman not found"
  if [[ " ${scenarios[*]} " != " selftest " ]]; then
    [[ -x $RJ_BIN ]] || die "CLI binary $RJ_BIN not found (build it, or set RJ_BIN)"
  fi

  trap cleanup EXIT
  trap 'exit 130' INT TERM
  mkdir -p "$OUT"
  build_images
  create_network
  generate_keys

  local failed=() rc
  for s in "${scenarios[@]}"; do
    rc=0
    if [[ $s = selftest ]]; then selftest || rc=$?; else run_scenario "$s" || rc=$?; fi
    if ((rc == 0)); then log "scenario $s: PASS"; else log "scenario $s: FAIL (artifacts in $OUT/$s)"; failed+=("$s"); fi
  done

  if ((${#failed[@]})); then
    log "failed: ${failed[*]}"
    exit 1
  fi
  log "all passed: ${scenarios[*]}"
}

[[ ${BASH_SOURCE[0]} != "$0" ]] || main "$@"
