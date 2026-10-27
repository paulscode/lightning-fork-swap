// Lightning Fork Swap: the client tag that the access log carries in place
// of the client's address (lightningfork.conf, js_set $lfswap_client_tag).
//
// The tag is an HMAC of the address under a key that scripts/log-key.sh
// replaces every day and does not keep. Within a day one client has one
// tag, so a bot or a flood still shows as one client; once the day's key is
// gone, nobody (the operator included) can tell which address a tag was,
// nor link the tags of different days.
//
// An IPv6 client is tagged by its /64, as the rate limits count it.

import crypto from 'crypto';

function tag(r) {
    var key = r.variables.lfswap_tag_key;
    var addr = r.rawVariables.binary_remote_addr;
    if (!key || !addr) {
        return '-';
    }
    if (addr.length == 16) {
        addr = addr.slice(0, 8);
    }
    return crypto.createHmac('sha256', key).update(addr).digest('hex').slice(0, 12);
}

export default {tag};
