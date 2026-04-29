# yunzes-node

A yunzes-node server based on multi core, modified from V2bX.  
一个基于多种内核的 yunzes-node 节点服务端，修改自 V2bX，支持 V2ray、Trojan、Shadowsocks、Tuic、Hysteria 协议。

## 软件安装

### 一键安装

```
wget -N https://raw.githubusercontent.com/husibo16/yunzes-node/master/Scripts/install.sh && bash install.sh
```

## 构建
``` bash
# 通过-tags选项指定要编译的内核， 可选 xray，sing
GOEXPERIMENT=jsonv2 go build -v -o ./node -tags "xray sing with_quic with_grpc with_utls with_wireguard with_acme" -trimpath -ldflags "-s -w -buildid="
```

## 容器部署 (Container deployment)

本阶段采用**单容器双核心模式**：xray-core 与 sing-box 都以 Go 库形式链接进同一个 `yunzes-node` 二进制；不拆成 xray / sing 独立容器。

### 路径约定

| 路径 | 用途 | 是否必须持久化 |
|---|---|---|
| `/etc/yunzes-node/config.json` | 节点配置入口 | 是 |
| `/etc/yunzes-node/certs/` | ACME / 自签证书 | 是（容器重建不能丢，否则会重复申请） |

容器内入口固定为：

```
yunzes-node server --config /etc/yunzes-node/config.json
```

### 构建镜像

```bash
docker build -t yunzes-node:test .
```

构建阶段已在 Dockerfile 内固定 `GOEXPERIMENT=jsonv2`，并带齐 build tags：
`sing xray with_quic with_grpc with_utls with_wireguard with_acme with_gvisor`。

### 启动 — host network（生产推荐，仅 Linux）

```bash
docker run -d \
    --name yunzes-node \
    --network host \
    --restart always \
    -v /etc/yunzes-node:/etc/yunzes-node \
    yunzes-node:test
```

或使用 docker-compose：

```bash
docker compose up -d
```

`docker-compose.yml` 已默认 `network_mode: host` + `restart: always` + 挂 `/etc/yunzes-node`。

`Scripts/docker-run.sh` 是一键脚本，等价于上面的 `docker run`，并会自动创建 host 目录、检查 `config.json` 是否存在、删除旧容器。

```bash
Scripts/docker-run.sh                # host network
Scripts/docker-run.sh --bridge       # 端口映射 fallback
Scripts/docker-run.sh --bridge --port 443:443/tcp --port 443:443/udp --port 80:80/tcp
```

### 启动 — bridge fallback（仅用于本地测试）

bridge 模式只适合**固定端口**的本地测试，**不推荐生产**。需要注意：

- ACME HTTP-01 需要把 `80/tcp` 映射出来（`-p 80:80/tcp`）
- Hysteria2 / TUIC 等 UDP 协议必须显式 `-p xxxx:xxxx/udp`，否则容器收不到 UDP
- bridge 默认会做 SNAT，客户端真实 IP 会被遮掉，除非你额外配 proxy-protocol
- 多端口 / 大量端口情况下手动 `-p` 易漏，host network 更适合

```bash
docker run -d \
    --name yunzes-node \
    --restart always \
    -p 80:80/tcp \
    -p 443:443/tcp \
    -p 443:443/udp \
    -v /etc/yunzes-node:/etc/yunzes-node \
    yunzes-node:test
```

### 持久化

`/etc/yunzes-node` 必须挂 host 目录，至少包含：

- `config.json` — 节点配置；可热更新（容器内 `--watch` 会自动 reload）
- `certs/` — 由 `EnsureCertificate` 写入 ACME 颁发的证书 + 私钥；首次启动后会出现 `*.crt` / `*.key`

容器重建只要 host 目录还在，证书会被复用：`EnsureCertificate` 在 reload / start 时 stat + parse X.509，未临近过期就 reuse，不会触发 ACME。`RenewBeforeDays`（默认 30）控制提前多少天续签，可在 `CertConfig` 里配。

### 平台差异

| 主机 | host network | 备注 |
|---|---|---|
| Linux | ✅ 真实可用 | 生产推荐 |
| Linux + WSL2 | ✅ 真实可用 | 在 Linux 子系统内执行 |
| Windows Docker Desktop | ⚠️ 名义可设但不通 | 容器跑在 WSL2 VM 里，宿主端口拿不到；只能做 build / 启动 / 配置加载 / 日志 等降级验证 |
| macOS Docker Desktop | ⚠️ 同上 | 同上 |

**真实的 host network 验证（80/tcp、UDP、host 端口绑定）必须在 Linux / WSL / VPS 上执行。**

### 日志

容器日志走 stdout/stderr，结构化字段：

```bash
docker logs -f yunzes-node
```

启动事件每条日志会带：
- `logical_tag` — `[apiHost]-protocol:server_id`，server-facing 标识
- `core` — `xray` 或 `sing`
- `runtime_key` — `core|logicalTag`，进程内 inbound tag / limiter map key
- `protocol` — vless / vmess / trojan / shadowsocks / hysteria2 / tuic / anytls
- `server_id` — 节点 ID
- `listen_addr` — 已归一（"" → `0.0.0.0`）
- `network` — `[tcp]` / `[udp]` / `[tcp udp]`（shadowsocks 双登记）
- `port` — listen 端口

证书事件额外带：
- `cert_action` — `issue` / `renew` / `reuse` / `reissue` / `error`
- `domain` / `cert_file` / `key_file`
- `remaining` — 距离 `NotAfter` 的时长
- `renew_before_days` — 实际生效的续签阈值

### 容器层验证清单

```bash
# 1. 镜像构建
docker build -t yunzes-node:test .

# 2. Windows Docker Desktop 降级（无 host 网络）
docker run --rm \
    -v /etc/yunzes-node:/etc/yunzes-node \
    yunzes-node:test

# 3. Linux / WSL / VPS 最终验证
docker run --rm --network host \
    -v /etc/yunzes-node:/etc/yunzes-node \
    yunzes-node:test
```

需要在 Linux 主机确认：

- `docker build` 成功
- 容器启动且 `docker logs` 看得到节点上线日志
- `ss -lntup | grep yunzes-node` 能看到 80/tcp、443/tcp、443/udp 等监听
- 容器重启后 `cert_action=reuse`（不是 `renew` 或 `issue`），证明证书目录被持久化
- Hysteria2 / TUIC 节点客户端能连上（验证 UDP 监听真的工作）
- `docker stop yunzes-node` 不 panic，`docker logs` 收尾干净（C2 反向 Close + nil-skip）

