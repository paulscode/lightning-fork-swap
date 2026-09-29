FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /txindex-shim .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /txindex-shim /txindex-shim
ENTRYPOINT ["/txindex-shim"]
