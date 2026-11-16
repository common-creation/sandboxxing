#!/bin/bash
# Guest system settings. The bootstrap applies them on the first boot, and
# sync-config runs this script again to bring an existing instance up to date,
# so every step must be idempotent.
set -eu
# Keep instance stops fast: units that ignore SIGTERM are killed after 10s
# instead of the 90s default. daemon-reload re-reads system.conf.d.
install -d -m 0755 /etc/systemd/system.conf.d
cat > /etc/systemd/system.conf.d/60-sandboxxing-timeout.conf <<'CONF'
[Manager]
DefaultTimeoutStopSec=10s
CONF
chmod 0644 /etc/systemd/system.conf.d/60-sandboxxing-timeout.conf
# Top-level drop-ins also cap units that set their own stop timeout, and
# mount/swap units, whose TimeoutSec= defaults to DefaultTimeoutStartSec=
# (an EFS unmount whose TLS proxy is gone otherwise waits 90s).
for unit in service:Service:TimeoutStopSec scope:Scope:TimeoutStopSec mount:Mount:TimeoutSec swap:Swap:TimeoutSec; do
    IFS=: read -r type section key <<<"$unit"
    install -d -m 0755 "/etc/systemd/system/$type.d"
    printf '[%s]\n%s=10s\n' "$section" "$key" > "/etc/systemd/system/$type.d/60-sandboxxing-timeout.conf"
    chmod 0644 "/etc/systemd/system/$type.d/60-sandboxxing-timeout.conf"
done
systemctl daemon-reload
