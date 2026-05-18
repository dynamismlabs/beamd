# syntax=docker/dockerfile:1.7
#
# Multi-stage build for conduitd. The final image ships only the
# static binary + CA roots — small surface, easy to inspect.

FROM golang:1.22-alpine AS build
WORKDIR /src

# Build deps first (cache-friendly).
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.Version=${VERSION}" \
        -o /out/conduitd ./cmd/conduitd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/conduitd /usr/local/bin/conduitd

# /etc/conduit holds the YAML config + tokens.json (bind-mounted in).
# /var/lib/conduit holds the ACME cache. Both are volumes so they
# survive container replacement.
VOLUME ["/etc/conduit", "/var/lib/conduit"]

EXPOSE 443

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/conduitd"]
CMD ["serve", "--config", "/etc/conduit/conduitd.yaml"]
