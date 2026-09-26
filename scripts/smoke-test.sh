#!/usr/bin/env bash
# sandboxxing end-to-end smoke test. Run as root on the host.
#
#   sudo ./scripts/smoke-test.sh
#
# It builds the base image with pacstrap, creates a container, verifies the
# SSH control interface and the direct container login, then removes the
# container again. The host network is configured with a dedicated bridge,
# subnet and port so the test does not disturb an existing installation.
set -euo pipefail

BIN=${BIN:-$(command -v sandboxxing || echo /tmp/opencode/sandboxxing)}
CONFIG=${CONFIG:-/tmp/opencode/smoke.json}
PORT=${PORT:-2223}
PASSWORD=${PASSWORD:-smoke-password}
WORKDIR=${WORKDIR:-/tmp/opencode/smoke}

if [[ $EUID -ne 0 ]]; then
    echo "run me as root" >&2
    exit 1
fi
if [[ ! -x $BIN ]]; then
    echo "daemon binary not found: $BIN" >&2
    exit 1
fi

echo "== writing configuration =="
mkdir -p "$WORKDIR"
cat >"$CONFIG" <<EOF
{
  "data_dir": "$WORKDIR/data",
  "ssh_addr": "127.0.0.1:$PORT",
  "password": "$PASSWORD",
  "bridge": "sbxsmoke0",
  "subnet": "10.222.0.0/24",
  "log_level": "debug",
  "boot_timeout": "5m"
}
EOF

echo "== host check =="
"$BIN" -config "$CONFIG" -check

echo "== starting daemon =="
"$BIN" -config "$CONFIG" >"$WORKDIR/daemon.log" 2>&1 &
DAEMON=$!
trap 'kill $DAEMON 2>/dev/null || true' EXIT
for _ in $(seq 1 60); do
    grep -q "listening for SSH connections" "$WORKDIR/daemon.log" && break
    sleep 0.5
done

# The daemon has no key based access in this test, so the password must be
# supplied non-interactively. sshpass is used when available; otherwise the
# standard SSH_ASKPASS helper is installed.
SSH_BIN=${SSH_BIN:-}
if command -v sshpass >/dev/null; then
    SSH_BIN="sshpass -p $PASSWORD ssh"
else
    cat >"$WORKDIR/askpass" <<EOF
#!/bin/sh
echo "$PASSWORD"
EOF
    chmod +x "$WORKDIR/askpass"
    export SSH_ASKPASS="$WORKDIR/askpass"
    export SSH_ASKPASS_REQUIRE=force
    export DISPLAY=:0
    SSH_BIN=ssh
fi

ssh_base() {
    $SSH_BIN -p "$PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o NumberOfPasswordPrompts=1 "$@"
}

run() { ssh_base sandbox@127.0.0.1 "$@"; }
run_vm() {
    local name=$1
    shift
    ssh_base "$name"@127.0.0.1 "$@"
}

echo "== ls (empty) =="
run ls

echo "== new =="
run new --name=demo --cpu=2 --memory=1G --disk=4G --comment="smoke test"

echo "== ls -l =="
run ls -l demo

echo "== direct login: command execution =="
run_vm demo uname -a
run_vm demo cat /etc/hostname
run_vm demo ip -4 addr show host0

echo "== stat =="
run stat demo

echo "== resize (live cpu and memory) =="
run resize demo --cpu=4 --memory=2G

echo "== restart =="
run restart demo
run_vm demo uptime

echo "== container has network access =="
run_vm demo timeout 20 getent hosts archlinux.org

echo "== cp =="
run cp demo demo-copy
run_vm demo-copy hostname

echo "== rm =="
run rm demo-copy demo

echo "== all smoke tests passed =="
