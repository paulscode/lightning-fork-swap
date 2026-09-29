FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /txindex-shim . && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /txindex-shim /txindex-shim
# Owned by the image's user, so a fresh named volume at /data is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENTRYPOINT ["/txindex-shim"]
