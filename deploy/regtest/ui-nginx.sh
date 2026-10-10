#!/usr/bin/env bash
# Serves a regtest build of the web app through the production nginx config,
# at https://localhost:18443, in front of the regtest backend (deploy/regtest):
# the same allowlist, headers and Content-Security-Policy as the live site,
# with a throwaway certificate and without the preview password.
#
#   ui-nginx.sh up DIST [GRAPH]
#                           DIST: the web app built with
#                           VITE_API_URL=https://localhost:18443 (regtest config);
#                           GRAPH: a graph generator's output (meta.json,
#                           v<N>/), served at /graph/
#   ui-nginx.sh down
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
name=lfswap-regtest-ui
NGINX_IMAGE=${NGINX_IMAGE:-nginx:1.22.1}
state=${TMPDIR:-/tmp}/$name

case "${1:-}" in
up)
	dist=$(cd "${2:?the web app build to serve}" && pwd)
	graph_mount=()
	if [ -n "${3:-}" ]; then
		graph_mount=(-v "$(cd "$3" && pwd):/sky/graph:ro")
	fi
	docker rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$state"
	mkdir -p "$state"
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=localhost" \
		-keyout "$state/site.key" -out "$state/site.crt" 2>/dev/null
	: > "$state/maintenance.conf"
	: > "$state/preview.conf"
	sed -e "s#/etc/letsencrypt/live/lightningfork.com/fullchain.pem#/t/site.crt#" \
		-e "s#/etc/letsencrypt/live/lightningfork.com/privkey.pem#/t/site.key#" \
		-e "s#/etc/nginx/lfswap-maintenance.conf#/t/maintenance.conf#" \
		-e "s#/etc/nginx/lfswap-preview.conf#/t/preview.conf#" \
		-e "s#root /srv/lfswap/webapp;#root /dist;#" \
		-e "s#root /srv/lfswap;#root /sky;#" \
		-e "s#server 127.0.0.1:9001#server 127.0.0.1:19001#" \
		-e "s#server 127.0.0.1:9004#server 127.0.0.1:19004#" \
		-e "s#server 127.0.0.1:9005#server 127.0.0.1:19005#" \
		-e "s#server 127.0.0.1:9010#server 127.0.0.1:19010#" \
		-e "s#listen 443 ssl http2;#listen 127.0.0.1:18443 ssl http2;#" \
		-e "s#listen 443 ssl default_server;#listen 127.0.0.1:18443 ssl default_server;#" \
		-e "/listen \[::\]/d" \
		-e "s#listen 80;#listen 127.0.0.1:18080;#" \
		-e "s#listen 80 default_server;#listen 127.0.0.1:18080 default_server;#" \
		-e "s#server_name lightningfork.com www.lightningfork.com;#server_name lightningfork.com www.lightningfork.com localhost;#" \
		-e "s#wss://lightningfork.com#wss://localhost:18443#" \
		"$here/../nginx/lightningfork.conf" > "$state/site.conf"
	chmod -R a+rX "$state"
	# Host networking: the regtest backend listens on the host's 127.0.0.1
	docker run -d --name "$name" --network host \
		-v "$state:/t:ro" -v "$dist:/dist:ro" "${graph_mount[@]}" \
		-v "$state/site.conf:/etc/nginx/conf.d/default.conf:ro" "$NGINX_IMAGE" >/dev/null
	sleep 1
	docker exec "$name" nginx -t 2>&1 | tail -1
	echo "https://localhost:18443"
	;;
down)
	docker rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$state"
	;;
*)
	echo "usage: $0 up DIST | down" >&2
	exit 1
	;;
esac
