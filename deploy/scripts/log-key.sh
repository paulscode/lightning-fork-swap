#!/usr/bin/env bash
# Replaces the key of the access log's client tags (nginx/lfswap_privacy.js)
# and reloads nginx, so the tags of the day before can no longer be traced
# back to an address. Run daily from cron (scripts/migrate/lib.sh) and once
# by install-host.sh nginx. The old key is overwritten, not kept.
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
if [ -e "$KEY_FILE" ]; then
	# Overwrite the old key in place before it is replaced
	shred -u "$KEY_FILE" 2>/dev/null || rm -f "$KEY_FILE"
fi
mv "$tmp" "$KEY_FILE"

if [ "${1:-}" != --if-none ]; then
	nginx -t -q && systemctl reload nginx
fi
