# Base images by digest: a tag can be moved to other contents
FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /txindex-shim . && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /txindex-shim /txindex-shim
# Owned by the image's user, so a fresh named volume at /data is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENTRYPOINT ["/txindex-shim"]
