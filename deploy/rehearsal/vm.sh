#!/usr/bin/env bash
# Throwaway Debian 12 VMs for rehearsing a migration, on this machine's KVM.
#
#   vm.sh create NAME [MEMORY_MB] [DISK_GB]   boot a VM from the Debian cloud image
#   vm.sh ip NAME                             its address on libvirt's default network
#   vm.sh destroy NAME                        remove it and its disk
#
# VMs get root SSH with the invoking user's key, plus a shared rehearsal key
# (in $WORK) so they can reach each other as two real hosts would during a
# migration. Needs libvirt (qemu:///system, the default network and pool)
# and genisoimage.
set -euo pipefail
export LIBVIRT_DEFAULT_URI=qemu:///system
WORK=${LFSWAP_VM_WORK:-/mnt/Black/lfswap-vms}
BASE_FILE=debian-12-genericcloud-amd64-20260923-2610.qcow2
BASE_VOL=lfswap-debian12-base.qcow2
POOL=default

cmd=${1:-}; name=${2:-}
[ -n "$cmd" ] && [ -n "$name" ] || { echo "usage: $0 create|ip|destroy NAME" >&2; exit 1; }

case "$cmd" in
create)
	mem=${3:-2048}; disk=${4:-30}
	[ -f "$WORK/$BASE_FILE" ] || { echo "missing $WORK/$BASE_FILE" >&2; exit 1; }
	[ -f "$WORK/rehearsal_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C lfswap-rehearsal -f "$WORK/rehearsal_ed25519"
	if ! virsh vol-info --pool $POOL $BASE_VOL >/dev/null 2>&1; then
		virsh vol-create-as $POOL $BASE_VOL 3G --format qcow2 >/dev/null
		virsh vol-upload --pool $POOL $BASE_VOL "$WORK/$BASE_FILE"
	fi
	virsh vol-create-as $POOL "$name.qcow2" "${disk}G" --format qcow2 \
		--backing-vol $BASE_VOL --backing-vol-format qcow2 >/dev/null

	seed=$(mktemp -d)
	cat > "$seed/meta-data" <<M
instance-id: $name
local-hostname: $name
M
	cat > "$seed/user-data" <<U
#cloud-config
disable_root: false
ssh_pwauth: false
users:
  - name: root
    ssh_authorized_keys:
      - $(cat ~/.ssh/id_ed25519.pub)
      - $(cat "$WORK/rehearsal_ed25519.pub")
write_files:
  - path: /root/.ssh/id_ed25519
    permissions: '0600'
    content: |
$(sed 's/^/      /' "$WORK/rehearsal_ed25519")
  - path: /root/.ssh/config
    permissions: '0600'
    content: |
      Host *
        StrictHostKeyChecking accept-new
growpart: {mode: auto, devices: ['/']}
U
	genisoimage -quiet -output "$seed/seed.iso" -volid cidata -joliet -rock "$seed/user-data" "$seed/meta-data"
	size=$(stat -c %s "$seed/seed.iso")
	virsh vol-create-as $POOL "$name-seed.iso" "$size" --format raw >/dev/null
	virsh vol-upload --pool $POOL "$name-seed.iso" "$seed/seed.iso"
	rm -rf "$seed"

	virt-install --name "$name" --memory "$mem" --vcpus 1 --import \
		--disk vol=$POOL/"$name.qcow2",bus=virtio \
		--disk vol=$POOL/"$name-seed.iso",device=disk,bus=virtio,readonly=on \
		--network network=default,model=virtio --os-variant debian12 \
		--graphics vnc,listen=127.0.0.1 --noautoconsole >/dev/null
	echo "created $name"
	;;
ip)
	for _ in $(seq 1 60); do
		ip=$(virsh domifaddr "$name" 2>/dev/null | awk '/ipv4/ {print $4}' | cut -d/ -f1 | head -1)
		if [ -n "$ip" ]; then echo "$ip"; exit 0; fi
		sleep 2
	done
	echo "no address for $name" >&2; exit 1
	;;
destroy)
	virsh destroy "$name" >/dev/null 2>&1 || true
	virsh undefine "$name" >/dev/null 2>&1 || true
	virsh vol-delete --pool $POOL "$name.qcow2" >/dev/null 2>&1 || true
	virsh vol-delete --pool $POOL "$name-seed.iso" >/dev/null 2>&1 || true
	echo "destroyed $name"
	;;
*) echo "usage: $0 create|ip|destroy NAME" >&2; exit 1 ;;
esac
