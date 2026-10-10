#!/usr/bin/env bash
# Rehearses moving Lightning Fork Swap between hosts, end to end, on three
# throwaway KVM VMs on this machine, in regtest. Never touches production.
#
#   lfswap-user  the world: a regtest Knots that mines, and the user's
#                Lightning Fork node
#   lfswap-src   the service, provisioned and set up with the real scripts
#   lfswap-dst   the host it moves to
#
# Stages (all by default, or name some):
#   setup      create the VMs, provision them, start the world and the service
#   migrate    move src -> dst with a reverse swap and a submarine swap in flight
#   rollback   move dst -> src again (--returning), a swap in flight
#   disaster   back up src, destroy it, restore on a wiped dst, recover funds
#   teardown   destroy the VMs
#
# Needs: libvirt/KVM, genisoimage, Node 24 and e2e/node_modules, and the
# backend image lfswap/boltz:dev in the local Docker.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
VM=$HERE/vm.sh
E2E=$REPO/e2e
export PATH=$HOME/.nvm/versions/node/v24.20.0/bin:$PATH
TUNNEL_PORT=19101
export API=http://127.0.0.1:$TUNNEL_PORT

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ip_of() { "$VM" ip "$1"; }
# Throwaway local VMs whose addresses are reused with new host keys: no
# known-hosts bookkeeping (in this script only).
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
on() { local host=$1; shift; ssh "${SSH_OPTS[@]}" "root@$host" "$@" </dev/null; }
svc() { on "$1" "cd /opt/lfswap/deploy && $2"; }
world() { on "$USR" "cd /opt/lfswap/deploy/rehearsal/user && $1"; }
WK='docker compose exec -T knots bitcoin-cli -regtest -rpcuser=lab -rpcpassword=lab'
WL='docker compose exec -T lnd-user lncli --network=regtest'
mine() { world "$WK generatetoaddress $1 \$($WK -rpcwallet=miner getnewaddress) >/dev/null"; }
swap_pub() { svc "$1" "docker compose exec -T lnd lncli --network=regtest getinfo" | jq -r .identity_pubkey; }
tunnel() {
	ps -eo pid,args | awk -v p="$TUNNEL_PORT:127.0.0.1:9001" '$2=="ssh" && index($0, p) {print $1}' | xargs -r kill
	sleep 1
	ssh "${SSH_OPTS[@]}" -f -N -o ExitOnForwardFailure=yes -L "$TUNNEL_PORT:127.0.0.1:9001" "root@$1"
}
client() { (cd "$E2E" && USER_VM=$USR timeout 900 node rehearsal.mjs "$@"); }
reconnect_user() {
	world "$WL connect $(swap_pub "$1")@$1:9735 >/dev/null 2>&1 || true"
	for _ in $(seq 1 30); do
		[ "$(world "$WL listchannels" | jq '[.channels[] | select(.active)] | length')" -ge 1 ] && return 0
		sleep 3
	done
	echo "user node did not reconnect to $1" >&2; return 1
}
open_channels() {
	local host=$1 upub spub
	upub=$(world "$WL getinfo" | jq -r .identity_pubkey)
	spub=$(swap_pub "$host")
	world "$WL connect $spub@$host:9735 >/dev/null 2>&1 || true"
	world "$WL openchannel --node_key $spub --local_amt 10000000 >/dev/null"
	mine 6; sleep 12
	svc "$host" "docker compose exec -T lnd lncli --network=regtest openchannel --node_key $upub --local_amt 10000000 >/dev/null"
	sleep 5; mine 6
	for _ in $(seq 1 40); do
		[ "$(world "$WL listchannels" | jq '[.channels[] | select(.active)] | length')" -ge 2 ] && return 0
		mine 1; sleep 5
	done
	echo "channels did not become active" >&2; return 1
}
wait_lnd() {
	for _ in $(seq 1 60); do
		svc "$1" "docker compose exec -T lnd lncli --network=regtest getinfo 2>/dev/null" | grep -Eq '"synced_to_chain": +true' && return 0
		sleep 5
	done
	echo "lnd on $1 did not come up" >&2; return 1
}
age_key() { svc "$1" "age-keygen -y /srv/lfswap/migration/host.agekey"; }
latest_archive() { svc "$1" "ls -t /srv/lfswap/migration/lfswap-*.tar.gz.age | head -1"; }

copy_repo() {
	tar -C "$REPO" --exclude=backend --exclude=webapp --exclude=references --exclude=internal_docs \
		--exclude=.git --exclude=node_modules --exclude=e2e -czf - . | ssh "${SSH_OPTS[@]}" "root@$1" 'mkdir -p /opt/lfswap && tar -xzf - -C /opt/lfswap'
}

stage_setup() {
	log "setup: VMs"
	for n in lfswap-user lfswap-src lfswap-dst; do "$VM" create "$n"; done
	USR=$(ip_of lfswap-user); SRC=$(ip_of lfswap-src); DST=$(ip_of lfswap-dst)
	for h in $USR $SRC $DST; do
		for _ in $(seq 1 30); do on "$h" true 2>/dev/null && break; sleep 3; done
	done
	log "setup: provision-host.sh on all three"
	for h in $USR $SRC $DST; do copy_repo "$h"; done
	for h in $USR $SRC $DST; do on "$h" 'cd /opt/lfswap/deploy && ./scripts/provision-host.sh --swap-gb 1 >/tmp/provision.log 2>&1' & done
	wait
	for h in $SRC $DST; do docker save lfswap/boltz:dev | gzip -1 | ssh "${SSH_OPTS[@]}" "root@$h" 'gunzip | docker load >/dev/null' & done
	wait

	log "setup: the world (miner and user node)"
	world "docker compose up -d >/dev/null 2>&1"
	sleep 15
	world "$WK -named createwallet wallet_name=miner load_on_startup=true >/dev/null"
	mine 150

	log "setup: the service on src, with the real scripts"
	svc "$SRC" "cp .env.example .env && chmod 600 .env && sed -i 's/^NETWORK=.*/NETWORK=regtest/; s/^PUBLIC_IP=.*/PUBLIC_IP=$SRC/' .env && echo KNOTS_ADDNODE=$USR:18444 >> .env && ./scripts/setup.sh >/dev/null && docker compose up -d knots >/dev/null 2>&1"
	sleep 20
	svc "$SRC" "./scripts/init-wallets.sh >/dev/null 2>&1"
	wait_lnd "$SRC"
	local baddr laddr uaddr
	baddr=$(svc "$SRC" "docker compose exec -T knots bitcoin-cli -datadir=/data -rpcwallet=boltz getnewaddress")
	laddr=$(svc "$SRC" "docker compose exec -T lnd lncli --network=regtest newaddress p2tr" | jq -r .address)
	uaddr=$(world "$WL newaddress p2tr" | jq -r .address)
	for a in $baddr $laddr $uaddr; do world "$WK -rpcwallet=miner sendtoaddress $a 5 >/dev/null"; done
	mine 6; sleep 15
	open_channels "$SRC"
	svc "$SRC" "docker compose up -d postgres shim boltz >/dev/null 2>&1"
	for _ in $(seq 1 30); do svc "$SRC" "curl -sf http://127.0.0.1:9001/version >/dev/null" && break; sleep 5; done
	setup_donations
	tunnel "$SRC"
	client reverse
	client submarine
}

# A donation address in lnd's wallet, the donation services, and one
# donation on record, which every later stage must keep. No explorer
# follows regtest: the checks find nothing (verdict unknown).
donations_on_record() { svc "$1" "docker compose exec -T postgres psql -U boltz -d donations -Atc 'select count(*) from replay_checks'"; }
setup_donations() {
	log "setup: donations"
	local addr
	addr=$(svc "$SRC" "docker compose exec -T lnd lncli --network=regtest newaddress p2tr" | jq -r .address)
	svc "$SRC" "sed -i 's/^DONATION_ADDRESS=.*/DONATION_ADDRESS=$addr/' .env && printf 'DONATIONS_BLAKE2B_EXPLORERS=http://127.0.0.1:9\nDONATIONS_SHA256_EXPLORERS=http://127.0.0.1:9\n' >> .env && ./scripts/init-donations-db.sh >/dev/null && docker compose up -d donations-worker donations-api >/dev/null 2>&1"
	world "$WK -rpcwallet=miner sendtoaddress $addr 0.01 >/dev/null"
	mine 1
	for _ in $(seq 1 30); do
		[ "$(donations_on_record "$SRC")" = 1 ] && break
		sleep 3
	done
	[ "$(donations_on_record "$SRC")" = 1 ] || { echo "FAIL: the donation was not recorded" >&2; exit 1; }
	svc "$SRC" "curl -sf http://127.0.0.1:9010/donate/v1/health >/dev/null" || { echo "FAIL: donations API" >&2; exit 1; }
	echo "  a donation on record, the API answers"
}

stage_migrate() {
	log "migrate: src -> dst, with swaps in flight"
	svc "$SRC" "./scripts/migrate/copy-chain.sh --pre root@$DST >/dev/null 2>&1"
	client reverse-start /tmp/reh-reverse.json
	client sub-start /tmp/reh-sub.json
	svc "$SRC" "./scripts/migrate/export.sh --check"
	svc "$SRC" "./scripts/migrate/export.sh --recipient $(age_key "$DST") >/dev/null 2>&1"
	svc "$SRC" "./scripts/migrate/copy-chain.sh --final root@$DST >/dev/null 2>&1"
	local a; a=$(latest_archive "$SRC")
	svc "$SRC" "scp -q $a $a.sha256 root@$DST:/srv/lfswap/migration/"
	svc "$DST" "./scripts/migrate/import.sh $a --public-ip $DST 2>&1 | grep -E '  (ok|DIFF)|complete|ERROR'"
	log "migrate: the old host refuses to start lnd"
	if svc "$SRC" "docker compose up -d lnd >/dev/null 2>&1"; then echo "FAIL: lnd started on the retired host" >&2; exit 1; fi
	echo "  refused"
	reconnect_user "$DST"
	tunnel "$DST"
	client reverse-finish /tmp/reh-reverse.json
	client sub-finish /tmp/reh-sub.json
	client reverse
}

stage_rollback() {
	log "rollback: dst -> src (--returning), a swap in flight"
	client reverse-start /tmp/reh-reverse2.json
	svc "$DST" "./scripts/migrate/export.sh --recipient $(age_key "$SRC") >/dev/null 2>&1"
	svc "$DST" "./scripts/migrate/copy-chain.sh --final root@$SRC >/dev/null 2>&1"
	local a; a=$(latest_archive "$DST")
	svc "$DST" "scp -q $a $a.sha256 root@$SRC:/srv/lfswap/migration/"
	svc "$SRC" "./scripts/migrate/import.sh $a --public-ip $SRC --returning 2>&1 | grep -E '  (ok|DIFF)|complete|ERROR'"
	reconnect_user "$SRC"
	tunnel "$SRC"
	client reverse-finish /tmp/reh-reverse2.json
	client submarine
}

stage_disaster() {
	log "disaster: offsite backup from src, then src is lost"
	local op
	op=$(on "$USR" 'age-keygen -o /root/operator.agekey 2>/dev/null; age-keygen -y /root/operator.agekey')
	on "$USR" 'mkdir -p /root/offsite'
	svc "$SRC" "sed -i '/^BACKUP_/d' .env && printf 'BACKUP_AGE_RECIPIENT=$op\nBACKUP_DEST=root@$USR:/root/offsite\n' >> .env && ./scripts/backup-offsite.sh >/dev/null 2>&1 && ./scripts/migrate/verify.sh snapshot /root/before.json >/dev/null"
	local before; before=$(svc "$SRC" "cat /root/before.json")
	LIBVIRT_DEFAULT_URI=qemu:///system virsh destroy lfswap-src >/dev/null
	echo "  src destroyed"
	log "disaster: restore on a wiped dst"
	svc "$DST" "docker compose down >/dev/null 2>&1; docker volume prune -f >/dev/null; rm -rf /srv/lfswap .env && ./scripts/provision-host.sh --swap-gb 0 >/dev/null 2>&1"
	on "$USR" "scp -q \$(ls -t /root/offsite/lfswap-backup-*.age | head -1) /root/operator.agekey root@$DST:/root/"
	svc "$DST" "./scripts/migrate/restore.sh \$(ls /root/lfswap-backup-*.age | head -1) --identity /root/operator.agekey --public-ip $DST 2>&1 | grep -E 'restored|ERROR'; shred -u /root/operator.agekey"
	local pub_before pub_after
	pub_before=$(jq -r .lnd.pubkey <<<"$before")
	pub_after=$(swap_pub "$DST")
	[ "$pub_before" = "$pub_after" ] || { echo "FAIL: identity changed" >&2; exit 1; }
	echo "  same lnd identity $pub_after"
	[ "$(svc "$DST" "docker compose exec -T knots bitcoin-cli -datadir=/data -rpcwallet=boltz getbalances" | jq .mine.trusted)" = "$(jq .knots_wallet.trusted <<<"$before")" ] \
		|| { echo "FAIL: Knots wallet balance differs" >&2; exit 1; }
	echo "  Knots wallet balance as backed up"
	[ "$(donations_on_record "$DST")" = "$(jq '.donations | length' <<<"$before")" ] \
		|| { echo "FAIL: donations on record differ" >&2; exit 1; }
	svc "$DST" "curl -sf http://127.0.0.1:9010/donate/v1/health >/dev/null" || { echo "FAIL: donations API" >&2; exit 1; }
	echo "  donations on record as backed up"
	# The peer still dials the lost host's address; point it at the new one,
	# as gossip would, until it has done its part (closing the channels).
	for _ in $(seq 1 20); do
		world "$WL connect $pub_after@$DST:9735 >/dev/null 2>&1 || true"
		mine 3; sleep 8
		[ "$(svc "$DST" "docker compose exec -T lnd lncli --network=regtest pendingchannels" | jq '(.waiting_close_channels|length) + (.pending_force_closing_channels|length)')" = 0 ] \
			&& [ "$(svc "$DST" "docker compose exec -T lnd lncli --network=regtest closedchannels" | jq '.channels|length')" -ge 1 ] && break
	done
	local onchain_before onchain_after channels_local
	onchain_before=$(jq -r .lnd.onchain <<<"$before")
	channels_local=$(jq '[.lnd.channels[].local_balance | tonumber] | add' <<<"$before")
	onchain_after=$(svc "$DST" "docker compose exec -T lnd lncli --network=regtest walletbalance" | jq -r .total_balance)
	echo "  lnd on chain: $onchain_before before, channels held $channels_local, now $onchain_after"
	[ $((onchain_after)) -gt $((onchain_before + channels_local - 50000)) ] || { echo "FAIL: channel funds not recovered" >&2; exit 1; }
	open_channels "$DST"
	tunnel "$DST"
	client reverse
	client submarine
}

stage_teardown() {
	for n in lfswap-user lfswap-src lfswap-dst; do "$VM" destroy "$n"; done
}

stages=${*:-"setup migrate rollback disaster"}
if [[ " $stages " != *" setup "* ]] && [ "$stages" != teardown ]; then
	USR=$(ip_of lfswap-user); SRC=$(ip_of lfswap-src); DST=$(ip_of lfswap-dst)
fi
for s in $stages; do "stage_$s"; done
log "rehearsal passed: $stages"
