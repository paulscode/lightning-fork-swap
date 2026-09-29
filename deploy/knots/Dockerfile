# Bitcoin Knots from the official release tarball, checked against the
# SHA256SUMS that Luke Dashjr and others signed (verify that file's signature
# yourself before bumping the version or hash here).
FROM debian:bookworm-slim

ARG KNOTS_VERSION=29.4.2.knots20260508
ARG KNOTS_SHA256=b59d0445a317e21a03dc29425db3aba79b27d5125230b1a2b1dce62e120827c5

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && curl -fsSLo /tmp/knots.tar.gz \
       "https://bitcoinknots.org/files/29.x/${KNOTS_VERSION}/bitcoin-${KNOTS_VERSION}-x86_64-linux-gnu.tar.gz" \
    && echo "${KNOTS_SHA256}  /tmp/knots.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/knots.tar.gz -C /tmp \
    && install -m 0755 /tmp/bitcoin-${KNOTS_VERSION}/bin/bitcoind /tmp/bitcoin-${KNOTS_VERSION}/bin/bitcoin-cli /usr/local/bin/ \
    && rm -rf /tmp/knots.tar.gz /tmp/bitcoin-${KNOTS_VERSION} \
    && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 1000 -m -d /data bitcoin

USER bitcoin
VOLUME /data
ENTRYPOINT ["bitcoind", "-datadir=/data", "-printtoconsole"]
