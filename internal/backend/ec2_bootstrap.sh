#!/bin/bash
# EC2 user data runs once. The systemd bootstrap retries transient apt/S3
# failures and starts the guest agent on every subsequent boot.
set -eu
umask 077
install -d -m 0700 /etc/sandboxxing
echo '@CONFIG@' | base64 -d > /etc/sandboxxing/agent.json
echo '@ASSETS@' | base64 -d > /etc/sandboxxing/assets.json
cat > /usr/local/sbin/sandboxxing-install-agent <<'INSTALL'
#!/bin/bash
set -eu
if [ -f /etc/sandboxxing/installed ]; then exit 0; fi
export DEBIAN_FRONTEND=noninteractive
# The instance role has no Session Manager permissions; drop the idle agent.
snap remove --purge amazon-ssm-agent || true
apt-get -o Acquire::Retries=3 update
apt-get -o DPkg::Lock::Timeout=180 install -y python3-boto3 openssh-sftp-server cloud-guest-utils e2fsprogs
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
/usr/bin/python3 - <<'PY'
import json
from urllib.parse import urlsplit
import boto3

with open("/etc/sandboxxing/assets.json") as f:
    cfg = json.load(f)
client = boto3.client("s3", region_name=cfg["region"])
for key, filename in (("agent", "agent"), ("efs", "amazon-efs-utils.deb")):
    if cfg[key]:
        uri = urlsplit(cfg[key])
        client.download_file(uri.netloc, uri.path[1:], filename)
PY
if [ -f amazon-efs-utils.deb ]; then
    apt-get -o DPkg::Lock::Timeout=180 install -y ./amazon-efs-utils.deb
fi
install -m 0755 agent /usr/local/bin/sandboxxing-agent
touch /etc/sandboxxing/installed
INSTALL
chmod 0700 /usr/local/sbin/sandboxxing-install-agent
cat > /etc/systemd/system/sandboxxing-agent.service <<'UNIT'
[Unit]
Description=Sandboxxing guest agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0
[Service]
Type=simple
ExecStartPre=/usr/local/sbin/sandboxxing-install-agent
ExecStart=/usr/local/bin/sandboxxing-agent -ec2-config /etc/sandboxxing/agent.json
Restart=on-failure
RestartSec=10
TimeoutStartSec=15min
[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 /etc/systemd/system/sandboxxing-agent.service
# The guest settings (ec2_guest_config.sh) are inserted here; they end with
# a daemon-reload that also picks up the agent unit.
@GUEST_CONFIG@
systemctl enable --now --no-block sandboxxing-agent.service
