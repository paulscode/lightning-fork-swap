#!/usr/bin/env bash
# copy-chain.sh --pre|--final root@NEW_HOST
#
# Copies the pruned chain (blocks and chainstate) to the new host, so its
# Knots does not have to sync from scratch, which takes weeks on a small
# host. Run from the old host, with SSH access to the new one.
#
#   --pre    while Knots runs, days ahead: most of the data, not consistent
#   --final  after export.sh has stopped Knots: the consistent remainder
. "$(dirname "$0")/lib.sh"
MODE=${1:-}
TARGET=${2:-}
[ -n "$TARGET" ] || die "usage: $0 --pre|--final root@NEW_HOST"
case "$MODE" in
--pre) ;;
--final)
	if running knots; then die "Knots is still running; --final needs it stopped (export.sh stops it)"; fi
	;;
*) die "usage: $0 --pre|--final root@NEW_HOST" ;;
esac

ssh "$TARGET" "mkdir -p $ROOT/$KNOTS_NET"
log "copying $ROOT/$KNOTS_NET/{blocks,chainstate} to $TARGET ($MODE)"
rsync -a --delete --numeric-ids --info=stats1 \
	"$ROOT/$KNOTS_NET/blocks" "$ROOT/$KNOTS_NET/chainstate" "$TARGET:$ROOT/$KNOTS_NET/"
ssh "$TARGET" "chown -R 1000:1000 $ROOT/knots"
log "done"
