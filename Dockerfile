# syntax=docker/dockerfile:1.7
#
# Multi-stage build for beamd. The final image ships only the
# static binary + CA roots — small surface, easy to inspect.

FROM golang:1.25.12-alpine AS build
WORKDIR /src

# Build deps first (cache-friendly).
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.Version=${VERSION}" \
        -o /out/beamd ./cmd/beamd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/beamd /usr/local/bin/beamd

# /etc/beamd holds the YAML config + tokens.json (bind-mounted in).
# /var/lib/beamd holds the ACME cache. Both are volumes so they
# survive container replacement.
VOLUME ["/etc/beamd", "/var/lib/beamd"]

# Public HTTPS and tuned TLS/yamux fallback use TCP. The preferred tunnel
# transport uses QUIC on the same numeric port over UDP.
EXPOSE 443/tcp 443/udp

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/beamd"]
CMD ["serve", "--config", "/etc/beamd/beamd.yaml"]
