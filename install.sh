#!/usr/bin/env bash
# Install or upgrade cdd on a Linux server with Docker and systemd. Run as root:
#
#   ./install.sh --public-url https://deploy.example.com [--version v1.0.0 | --binary-dir DIR]
#               [--listen 127.0.0.1:8790]
#
# Touches only its own files: /usr/local/bin/{cdd,cdctl}, /etc/cdd, /var/lib/cdd,
# /etc/systemd/system/cdd.service. Does not change the firewall, the reverse proxy or Docker.
set -euo pipefail

REPO=Dmitry-Fedotov-Dev/cd
version=""
bindir=""
public_url=""
listen=127.0.0.1:8790

while [ $# -gt 0 ]; do
  case "$1" in
    --version) version=$2; shift 2 ;;
    --binary-dir) bindir=$2; shift 2 ;;
    --public-url) public_url=${2%/}; shift 2 ;;
    --listen) listen=$2; shift 2 ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
command -v docker >/dev/null || { echo "docker is not installed" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd is required" >&2; exit 1; }
if [ ! -f /etc/cdd/cdd.json ] && [ -z "$public_url" ]; then
  echo "--public-url is required on first install (the URL CI will call, e.g. https://deploy.example.com)" >&2
  exit 2
fi

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -z "$bindir" ]; then
  if [ -z "$version" ]; then
    version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
  fi
  [ -n "$version" ] || { echo "cannot find the latest release; pass --version" >&2; exit 1; }
  base="https://github.com/$REPO/releases/download/$version"
  file="cd_${version#v}_linux_${arch}.tar.gz"
  echo "==> download $file"
  curl -fsSL "$base/$file" -o "$tmp/$file"
  curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
  (cd "$tmp" && grep " $file\$" checksums.txt | sha256sum -c -) || { echo "checksum mismatch" >&2; exit 1; }
  tar -xzf "$tmp/$file" -C "$tmp"
  bindir=$tmp
fi
for b in cdd cdctl; do
  [ -x "$bindir/$b" ] || { echo "$bindir/$b not found" >&2; exit 1; }
done

echo "==> binaries"
install -m 0755 "$bindir/cdd" /usr/local/bin/cdd.new && mv /usr/local/bin/cdd.new /usr/local/bin/cdd
install -m 0755 "$bindir/cdctl" /usr/local/bin/cdctl
/usr/local/bin/cdd -version

echo "==> config"
install -d -m 0750 /etc/cdd /etc/cdd/projects /var/lib/cdd
if [ ! -f /etc/cdd/cdd.json ]; then
  cat >/etc/cdd/cdd.json <<EOF
{
  "listen": "$listen",
  "public_url": "$public_url",
  "issuers": ["https://token.actions.githubusercontent.com", "https://gitlab.com"],
  "projects_dir": "/etc/cdd/projects",
  "state_dir": "/var/lib/cdd",
  "build_min_free_mb": 700
}
EOF
  chmod 0640 /etc/cdd/cdd.json
  echo "created /etc/cdd/cdd.json"
else
  echo "kept /etc/cdd/cdd.json"
fi

echo "==> systemd"
cat >/etc/systemd/system/cdd.service <<'EOF'
[Unit]
Description=cd deploy daemon (cdd)
Documentation=https://github.com/Dmitry-Fedotov-Dev/cd
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/cdd -config /etc/cdd/cdd.json
Restart=on-failure
RestartSec=5
Environment=HOME=/var/lib/cdd DOCKER_CONFIG=/var/lib/cdd/.docker
# a deploy in progress finishes its swap before the daemon stops
TimeoutStopSec=11min
KillMode=mixed
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable cdd >/dev/null
systemctl restart cdd
sleep 1
systemctl is-active cdd

url=$(sed -n 's/.*"public_url": *"\([^"]*\)".*/\1/p' /etc/cdd/cdd.json)
addr=$(sed -n 's/.*"listen": *"\([^"]*\)".*/\1/p' /etc/cdd/cdd.json)
cat <<EOF

cdd is running on $addr. Publish it through the reverse proxy you already have, e.g. Caddy:

${url#https://} {
	request_body {
		max_size 60MB
	}
	reverse_proxy $addr {
		flush_interval -1
	}
}

Then allow a project:  cdctl project add NAME --repo OWNER/REPO --domain NAME.example.com
EOF
