# syntax=docker/dockerfile:1.6
#
# Single-container, dual-core build (xray + sing-box linked into one binary).
# The runtime image is pinned to a specific Alpine minor so a stale image cache
# can't pick up a breaking base-image rev.

FROM golang:1.25.0-alpine AS builder
WORKDIR /app

# encoding/json/v2 is a Go 1.25 experiment; the project's build constraints
# require it both for `go mod download` (transitive deps inspect json/v2) and
# for `go build` itself.
ENV CGO_ENABLED=0 GOEXPERIMENT=jsonv2

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -v \
        -tags "sing xray with_quic with_grpc with_utls with_wireguard with_acme with_gvisor" \
        -trimpath \
        -ldflags "-s -w -buildid=" \
        -o yunzes-node

FROM alpine:3.20

RUN apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

# /etc/yunzes-node holds config.json plus the certs/ subdir. A host bind-mount
# typically shadows this directory, but we still pre-create the structure so a
# fresh `docker run` without a volume still has the expected layout.
RUN mkdir -p /etc/yunzes-node/certs

COPY --from=builder /app/yunzes-node /usr/local/bin/

VOLUME ["/etc/yunzes-node"]

ENTRYPOINT ["yunzes-node", "server", "--config", "/etc/yunzes-node/config.json"]
