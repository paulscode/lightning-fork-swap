#!/usr/bin/env bash
# provision-host.sh [--swap-gb N]
#
# Takes a fresh Debian 12 host to the point where it can receive the service's
# state (scripts/migrate/import.sh) or start a new service (the first-install
# steps in deploy/README.md). Run as root from /opt/lfswap/deploy after
# copying the repository there. Safe to run again.
#
# Does: packages, Docker CE, swap, sysctl, the service's directories, the
# three small images built from source (the backend image is shipped
# separately, it is too heavy to build on a small host), and an age key for
# receiving a migration archive. Does not touch the firewall (run
# install-host.sh firewall and confirm from a second SSH session) and does not
# install the nginx site (import.sh does, with the moved certificate).
set -euo pipefail
cd "$(dirname "$0")/.."
SWAP_GB=3
while [ $# -gt 0 ]; do
	case "$1" in
	--swap-gb) SWAP_GB=$2; shift ;;
	*) echo "unknown argument $1" >&2; exit 1 ;;
	esac
	shift
done

. /etc/os-release
[ "$ID" = debian ] || echo "warning: tested on Debian 12, this is $PRETTY_NAME" >&2

export DEBIAN_FRONTEND=noninteractive
echo "== packages"
apt-get update -q
apt-get install -y -q ca-certificates curl gnupg nftables nginx certbot apache2-utils \
	jq rsync age unattended-upgrades >/dev/null

if ! command -v docker >/dev/null; then
	echo "== Docker CE"
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $VERSION_CODENAME stable" \
		> /etc/apt/sources.list.d/docker.list
	apt-get update -q
	apt-get install -y -q docker-ce docker-ce-cli containerd.io docker-compose-plugin >/dev/null
fi
# SSH by key only: bots try passwords on every public host all day. Only
# once root has a key, so that this cannot lock anyone out.
if [ -s /root/.ssh/authorized_keys ]; then
	cat > /etc/ssh/sshd_config.d/00-lfswap.conf <<'SSHD'
# Lightning Fork Swap: keys only. Read before sshd_config (Include is at its
# top and the first value of a setting wins).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
PermitEmptyPasswords no
X11Forwarding no
MaxAuthTries 3
SSHD
	sshd -t && systemctl reload ssh
else
	echo "root has no SSH key: password logins left on; add a key and run again" >&2
fi

# The journal keeps at most 200 MB (it grew past 1 GB on a 40 GB disk)
mkdir -p /etc/systemd/journald.conf.d
printf '[Journal]\nSystemMaxUse=200M\n' > /etc/systemd/journald.conf.d/lfswap.conf
systemctl restart systemd-journald

# On shutdown Docker gives containers 15 s and systemd gives Docker 90 s;
# Knots needs up to 5 minutes to stop cleanly (stop_grace_period), and a
# pruned node stopped hard may have to download the chain again.
mkdir -p /etc/systemd/system/docker.service.d
DROPIN=/etc/systemd/system/docker.service.d/lfswap-stop.conf
if [ "$(cat "$DROPIN" 2>/dev/null)" != "$(printf '[Service]\nTimeoutStopSec=7min')" ]; then
	printf '[Service]\nTimeoutStopSec=7min\n' > "$DROPIN"
	systemctl daemon-reload
fi
# Change Docker's configuration without restarting it where containers run:
# a restart would restart every one of them (shutdown-timeout is reloadable;
# the log options apply to containers created afterwards).
DAEMON_JSON='{ "log-driver": "json-file", "log-opts": { "max-size": "20m", "max-file": "3" }, "shutdown-timeout": 300 }'
if [ "$(jq -cS . /etc/docker/daemon.json 2>/dev/null)" != "$(echo "$DAEMON_JSON" | jq -cS .)" ]; then
	echo "$DAEMON_JSON" > /etc/docker/daemon.json
	if [ -n "$(docker ps -q 2>/dev/null)" ]; then
		systemctl reload docker
	else
		systemctl restart docker
	fi
fi

if [ "$SWAP_GB" -gt 0 ] && ! swapon --show=NAME --noheadings | grep -q /swapfile-lfswap; then
	echo "== ${SWAP_GB} GB swap"
	fallocate -l "${SWAP_GB}G" /swapfile-lfswap
	chmod 600 /swapfile-lfswap
	mkswap /swapfile-lfswap >/dev/null
	swapon /swapfile-lfswap
	grep -q /swapfile-lfswap /etc/fstab || echo '/swapfile-lfswap none swap sw 0 0' >> /etc/fstab
fi
echo 'vm.swappiness=10' > /etc/sysctl.d/99-lfswap.conf
sysctl -q -p /etc/sysctl.d/99-lfswap.conf

echo "== directories"
ROOT=/srv/lfswap
mkdir -p "$ROOT"/{knots/data,shim,tor/data,lnd,postgres,boltz,secrets,webapp,migration}
chmod 700 "$ROOT/secrets" "$ROOT/migration"
touch /etc/nginx/lfswap-maintenance.conf

echo "== images"
docker build -q -t lfswap/knots:29.4.2 knots >/dev/null
docker build -q -t lfswap/tor:dev tor >/dev/null
docker build -q -t lfswap/txindex-shim:dev ../shim >/dev/null
docker build -q -t lfswap/donations:dev ../donations >/dev/null
docker image inspect lfswap/boltz:dev >/dev/null 2>&1 || \
	echo "   the backend image is missing: docker save lfswap/boltz:dev | gzip | ssh root@THIS_HOST 'gunzip | docker load'"

KEY=$ROOT/migration/host.agekey
if [ ! -f "$KEY" ]; then
	echo "== age key for receiving a migration archive"
	age-keygen -o "$KEY" 2>/dev/null
	chmod 600 "$KEY"
fi

cat <<MSG

Host provisioned. To receive the service from another host, give that host's
export.sh this recipient:

  $(age-keygen -y "$KEY")

Still to do by hand: ./scripts/install-host.sh firewall (then firewall-confirm
from a second SSH session).
MSG
