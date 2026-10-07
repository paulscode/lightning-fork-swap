#!/usr/bin/env bash
# One-time host setup for Lightning Fork Swap on Debian 12, run as root from
# deploy/. Docker, nginx, certbot and nftables must already be installed
# (apt-get install nginx certbot nftables apache2-utils, and Docker CE).
#
#   install-host.sh firewall   install the nftables rules (with a 2-minute
#                              automatic undo unless confirmed)
#   install-host.sh tls        obtain the Let's Encrypt certificate
#   install-host.sh nginx      install the site and a preview password
set -euo pipefail
cd "$(dirname "$0")/.."
DOMAIN=${DOMAIN:-lightningfork.com}

case "${1:-}" in
firewall)
	install -m 0755 nftables.conf /etc/nftables.conf
	nft -f /etc/nftables.conf
	echo "Rules active. Undoing them in 120 s unless you run: $0 firewall-confirm"
	( sleep 120; if [ -f /run/lfswap-fw-pending ]; then nft delete table inet lfswap; echo "firewall undone"; fi ) >/var/log/lfswap-fw.log 2>&1 &
	touch /run/lfswap-fw-pending
	;;
firewall-confirm)
	rm -f /run/lfswap-fw-pending
	systemctl enable nftables >/dev/null 2>&1
	echo "firewall kept, and loaded at boot"
	;;
tls)
	mkdir -p /var/www/html
	certbot certonly --webroot -w /var/www/html -d "$DOMAIN" -d "www.$DOMAIN" \
		--non-interactive --agree-tos --register-unsafely-without-email
	# Reload nginx whenever the certificate is renewed.
	install -d /etc/letsencrypt/renewal-hooks/deploy
	printf '#!/bin/sh\nsystemctl reload nginx\n' > /etc/letsencrypt/renewal-hooks/deploy/reload-nginx
	chmod +x /etc/letsencrypt/renewal-hooks/deploy/reload-nginx
	;;
nginx)
	if [ ! -f /etc/nginx/lfswap.htpasswd ]; then
		pw=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)
		htpasswd -bc /etc/nginx/lfswap.htpasswd preview "$pw" >/dev/null
		chown root:www-data /etc/nginx/lfswap.htpasswd
		chmod 640 /etc/nginx/lfswap.htpasswd
		mkdir -p /srv/lfswap/secrets && chmod 700 /srv/lfswap/secrets
		echo "user preview, password $pw" > /srv/lfswap/secrets/preview-password.txt
		chmod 600 /srv/lfswap/secrets/preview-password.txt
		echo "preview password written to /srv/lfswap/secrets/preview-password.txt"
	fi
	touch /etc/nginx/lfswap-maintenance.conf
	# Created once, with the password on; going public empties it, and a
	# later run leaves it as it is
	if [ ! -e /etc/nginx/lfswap-preview.conf ]; then
		printf 'auth_basic "Lightning Fork Swap preview";\nauth_basic_user_file /etc/nginx/lfswap.htpasswd;\n' \
			> /etc/nginx/lfswap-preview.conf
	fi
	install -m 0644 nginx/lightningfork.conf /etc/nginx/sites-available/lightningfork.conf
	ln -sf /etc/nginx/sites-available/lightningfork.conf /etc/nginx/sites-enabled/lightningfork.conf
	rm -f /etc/nginx/sites-enabled/default
	nginx -t
	# A restart, not a reload: a reload that fails at runtime (a changed
	# limit_req zone, say) passes nginx -t and silently keeps the old config.
	systemctl restart nginx
	;;
*)
	echo "usage: $0 firewall|firewall-confirm|tls|nginx" >&2
	exit 1
	;;
esac
