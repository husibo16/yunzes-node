# Build go
FROM golang:1.25.0-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0
RUN GOEXPERIMENT=jsonv2 go mod download
RUN GOEXPERIMENT=jsonv2 go build -v -o yunzes-node -tags "sing xray with_quic with_grpc with_utls with_wireguard with_acme with_gvisor"

# Release
FROM  alpine
# 安装必要的工具包
RUN  apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir -p /etc/yunzes-node/certs
COPY --from=builder /app/yunzes-node /usr/local/bin/

VOLUME ["/etc/yunzes-node"]

ENTRYPOINT ["yunzes-node", "server", "--config", "/etc/yunzes-node/config.json"]
