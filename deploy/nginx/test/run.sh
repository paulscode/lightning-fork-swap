#!/usr/bin/env bash
# Tests deploy/nginx/lightningfork.conf in the nginx the host runs (Debian 12:
# 1.22.1), against mock backends and a mock explorer, on a throwaway Docker
# network. Nothing leaves the workstation.
#
#   deploy/nginx/test/run.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
NGINX_IMAGE=${NGINX_IMAGE:-nginx:1.22.1}
PYTHON_IMAGE=${PYTHON_IMAGE:-python:3-alpine}
id=lfswap-nginx-test-$$
work=$(mktemp -d)
cleanup() {
	docker rm -f "$id-mock" "$id-nginx" "$id-untrusted" >/dev/null 2>&1 || true
	docker network rm "$id" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# Certificates: a test CA for the explorer, one nobody trusts, and the site's
cd "$work"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=test CA" \
	-keyout ca.key -out ca.crt 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=mempool.guide" \
	-addext "subjectAltName=DNS:mempool.guide" -keyout explorer.key -out explorer.csr 2>/dev/null
openssl x509 -req -in explorer.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 \
	-copy_extensions copyall -out explorer.crt 2>/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=mempool.guide" \
	-addext "subjectAltName=DNS:mempool.guide" -keyout untrusted.key -out untrusted.crt 2>/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=lightningfork.com" \
	-keyout site.key -out site.crt 2>/dev/null
printf 'preview:%s\n' "$(openssl passwd -apr1 test)" > htpasswd
printf 'auth_basic "preview";\nauth_basic_user_file /t/htpasswd;\n' > preview.conf
mkdir -p webapp
echo '<!doctype html><title>app</title>' > webapp/index.html
: > maintenance.conf
chmod -R a+rX "$work"

# The production config, pointed at the mocks
render() {
	local explorer_port=$1
	sed -e "s#/etc/letsencrypt/live/lightningfork.com/fullchain.pem#/t/site.crt#" \
		-e "s#/etc/letsencrypt/live/lightningfork.com/privkey.pem#/t/site.key#" \
		-e "s#/etc/nginx/lfswap-preview.conf#/t/preview.conf#" \
		-e "s#/etc/nginx/lfswap-maintenance.conf#/t/maintenance.conf#" \
		-e "s#/etc/ssl/certs/ca-certificates.crt#/t/ca.crt#" \
		-e "s#root /srv/lfswap/webapp;#root /t/webapp;#" \
		-e "s#server 127.0.0.1:\(900[145]\)#server mock:\1#" \
		-e "s#https://mempool.guide/api/#https://mempool.guide:$explorer_port/api/#" \
		"$here/../lightningfork.conf"
}
render 8443 > site.conf
render 9443 > site-untrusted.conf

docker network create "$id" >/dev/null
docker run -d --name "$id-mock" --network "$id" --network-alias mock \
	--network-alias mempool.guide -v "$work:/t:ro" -v "$here/mock.py:/mock.py:ro" \
	"$PYTHON_IMAGE" python3 /mock.py /t >/dev/null
for n in nginx untrusted; do
	conf=site.conf
	[ "$n" = untrusted ] && conf=site-untrusted.conf
	alias=nginx
	[ "$n" = untrusted ] && alias=nginx-untrusted
	docker run -d --name "$id-$n" --network "$id" --network-alias "$alias" \
		-v "$work:/t:ro" -v "$work/$conf:/etc/nginx/conf.d/default.conf:ro" \
		"$NGINX_IMAGE" >/dev/null
done
sleep 2
for n in nginx untrusted; do
	if ! docker exec "$id-$n" nginx -t >/dev/null 2>&1; then
		docker exec "$id-$n" nginx -t || true
		docker logs "$id-$n" 2>&1 | tail -20
		exit 1
	fi
done

docker run --rm --network "$id" -v "$here/tests.py:/tests.py:ro" "$PYTHON_IMAGE" \
	python3 /tests.py
