#!/usr/bin/env bash
# Replaces the key of the access log's client tags (nginx/lfswap_privacy.js)
# and reloads nginx, so the tags of the day before can no longer be traced
# back to an address. Run daily from cron (scripts/migrate/lib.sh) and once
# by install-host.sh nginx. The old key is not kept.
#
#   log-key.sh            a new key, then reload nginx
#   log-key.sh --if-none  only when there is no key yet (no reload)
set -euo pipefail
KEY_FILE=${LFSWAP_TAG_KEY_FILE:-/etc/nginx/lfswap-tag-key.conf}

if [ "${1:-}" = --if-none ] && [ -s "$KEY_FILE" ]; then
	exit 0
fi

tmp=$(mktemp "$KEY_FILE.XXXXXX")
chmod 600 "$tmp"
printf 'js_var $lfswap_tag_key "%s";\n' "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$tmp"
# The new key takes the old one's place in one rename: the file nginx
# includes is never missing (a reload or a boot at that moment would fail).
# The old key's blocks are freed, not overwritten: on a journaling
# filesystem overwriting in place (shred) does not reach every copy anyway.
mv "$tmp" "$KEY_FILE"

if [ "${1:-}" != --if-none ]; then
	# nginx -t resolves the explorer's name: a DNS hiccup at midnight must
	# not leave yesterday's key in use. The monitor alerts on a key file
	# older than a day (check_jobs).
	for attempt in 1 2 3 4 5; do
		if nginx -t -q 2>/dev/null && systemctl reload nginx; then
			exit 0
		fi
		sleep 60
	done
	echo "log-key: nginx did not take the new key" >&2
	exit 1
fi
