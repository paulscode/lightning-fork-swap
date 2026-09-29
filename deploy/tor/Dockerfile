FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends tor \
    && rm -rf /var/lib/apt/lists/*
USER debian-tor
ENTRYPOINT ["tor", "-f", "/etc/tor/torrc"]
