#!/usr/bin/env bash
# maintenance.sh on|off|status
#
# On: nginx answers 503 to new swaps (POST to /v2/swap/submarine|reverse).
# Everything a user needs for swaps already under way keeps working: status,
# claims, refunds, the rescue page.
. "$(dirname "$0")/lib.sh"
FILE=/etc/nginx/lfswap-maintenance.conf
case "${1:-status}" in
on)
	echo 'if ($request_method = POST) { return 503; }' > "$FILE"
	nginx -t -q && systemctl reload nginx
	log "maintenance on: new swaps refused"
	;;
off)
	: > "$FILE"
	nginx -t -q && systemctl reload nginx
	log "maintenance off"
	;;
status)
	if [ -s "$FILE" ]; then echo on; else echo off; fi
	;;
*) die "usage: $0 on|off|status" ;;
esac
