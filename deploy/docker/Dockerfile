# Gateway container. Build from the repository root:
#   docker build -f deploy/docker/Dockerfile -t kloudit-recon-gateway .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
# go.mod replaces quic-go with the patched copy in third_party/quic-go.
COPY third_party/quic-go/go.mod third_party/quic-go/go.sum third_party/quic-go/
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/karamkamal1/kloudit-recon/internal/gateway.Version=${VERSION}" \
    -o /out/recon-gateway ./cmd/recon-gateway && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/recon-gateway /recon-gateway
# Owned by the non-root user so a fresh named volume is writable.
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME ["/data"]
EXPOSE 8443/tcp 8443/udp
ENTRYPOINT ["/recon-gateway", "-data", "/data"]
