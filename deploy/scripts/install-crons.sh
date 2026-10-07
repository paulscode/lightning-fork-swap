#!/usr/bin/env bash
# Installs this host's scheduled jobs (/etc/cron.d/lfswap-*): the hourly copy
# of the recovery files, the alert monitor every five minutes, and the daily
# offsite backup when BACKUP_AGE_RECIPIENT is set. The migration scripts do
# the same on the host the service moves to; run this after changing .env or
# on a host installed by hand. Safe to run again.
# shellcheck source=migrate/lib.sh
. "$(dirname "$0")/migrate/lib.sh"
refuse_if_migrated
install_crons
ls -l /etc/cron.d/lfswap-*
