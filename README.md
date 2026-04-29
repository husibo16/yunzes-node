# yunzes-node

A yunzes-node server based on multi core, modified from V2bX.  
一个基于多种内核的 yunzes-node 节点服务端，修改自 V2bX，支持 V2ray、Trojan、Shadowsocks、Tuic、Hysteria 协议。

## 一键安装与运维（推荐）

`Scripts/yunzes-node.sh` 是完整运维入口，安装后可直接以 `yunzes-node` 命令调用。它默认走 **Docker + host network + 单容器双核心** 模式，并把 PreCheck（23 项）、备份、回滚、卸载、fake panel 4 协议自测都串在一起。

```bash
# 1. 拉源码 + 进入目录
git clone https://github.com/husibo16/yunzes-node.git
cd yunzes-node

# 2. 安装命令入口（写到 /usr/bin/yunzes-node）
sudo bash Scripts/yunzes-node.sh setup-entry

# 3. 进入交互菜单
sudo yunzes-node
```

也支持非交互子命令，每个菜单项都有对应的 CLI：

```bash
sudo yunzes-node install               # 装并启动
sudo yunzes-node install --no-restart  # 装但不启 restart=always（用于先 verify）
sudo yunzes-node verify                # 三级验证：基础 / 网络 / 业务
sudo yunzes-node logs                  # 最近 100 行
sudo yunzes-node follow-log            # 跟随日志
sudo yunzes-node ports                 # ss 看 yunzes-node PID 的监听端口
sudo yunzes-node check-panel           # curl 探活每个节点的 panel API
sudo yunzes-node fake-test             # 起 fake panel + 4 协议自测
sudo yunzes-node show-config           # 自动隐藏 ApiKey 显示
sudo yunzes-node edit-config           # 自动备份再编辑，JSON 校验失败不重启
sudo yunzes-node backup                # 一次完整备份（config + certs + 镜像 ID）
sudo yunzes-node rollback              # 列出备份并选一个回滚
sudo yunzes-node update                # build/pull 新镜像 + 自动 PostCheck，失败自动回滚
sudo yunzes-node uninstall             # 删容器/镜像，保留配置和证书
sudo yunzes-node uninstall-full        # 全清；要求二次输入 "DELETE YUNZES NODE"
```

完整命令清单：`yunzes-node help`。

### 推荐生产端口方案

```
443/tcp      → vless + reality
8388/tcp+udp → shadowsocks       (脚本会自动同时登记 tcp+udp)
8443/udp     → hysteria2
9443/tcp     → trojan
```

端口规则（脚本和 Go 端 portRegistry 都强制执行）：

- 同 `transport` (tcp / udp) + 同 port → 冲突；
- `0.0.0.0:p/T` 与具体 IPv4:p/T → 冲突（wildcard 覆盖具体）；
- `::` 与 `0.0.0.0` 同 p+T → 冲突（dual-stack 不一致）；
- `443/tcp` 与 `443/udp` → 不冲突，可共存（reality + hysteria2 同 443 的常见做法）；
- shadowsocks 默认 tcp+udp 双登记，因此不能同端口再放 hysteria2。

### fake panel 四协议自测

`yunzes-node fake-test` 用于离线验证整套 C0~C5 行为，不需要真实 panel：

- 启动一个 Python `127.0.0.1:9999` fake panel，返回 4 协议的 NodeInfo
- 写入临时 4 协议测试配置（vless+tls / shadowsocks / hysteria2 / vless+reality）
- 用 `--restart no` 起容器（避免崩了无限重启刷屏）
- 检查日志无 `panic` / `nil pointer dereference` / `runtime error`
- 检查日志含 `Core Selector` / `Adding node inbound` / `logical_tag` / `core=` / `runtime_key` / `protocol=` / `server_id` / `port=` 字段
- 用 `ss -lntup` 校验 8101/tcp、8102/tcp、8102/udp、8103/udp、8104/tcp 五个监听都由 yunzes-node 占用
- 重启容器后日志含 `cert_action=reuse`（证明 C3 持久化生效）

跑完询问是否恢复原 config / 停 fake panel / 删测试容器 / 删测试证书。**默认不会动你已有的 ApiKey 和真实证书**，只在收尾时按你 y/N 选择处理。

### 升级与回滚

`yunzes-node update` 走如下流程：

1. PreCheck（同安装）
2. 备份当前 config.json / certs / `docker inspect` / 镜像 ID 到 `/opt/yunzes-node/backups/<timestamp>/`
3. 重新 build（源码可用时）或 pull 镜像
4. 删旧容器、起新容器
5. PostCheck —— L1 失败立即触发**自动回滚**：
   - 用旧镜像 ID 起回去
   - 还原 config + certs
   - 重跑一次 PostCheck

升级失败不会出现"容器没了 / 配置丢了 / 证书丢了"的状态，最坏情况是版本仍在升级前。

手动回滚到任一历史备份：`yunzes-node rollback`。

### 卸载

- `yunzes-node uninstall` — 删容器、可选删镜像、可选删 `/usr/bin/yunzes-node`，**保留** `/etc/yunzes-node` + 备份。
- `yunzes-node uninstall-full` — 危险操作，要求输入 `DELETE YUNZES NODE` 二次确认，删除所有容器 + 镜像 + 配置 + 证书 + 备份 + 全局命令。

### 常见问题排查

| 现象 | 原因 | 处理 |
|---|---|---|
| `permission denied while trying to connect to the docker API` | 当前用户不在 docker 组 | `sudo usermod -aG docker $USER`，重开终端；或直接用 root 跑脚本 |
| `cert_action=error  invalid reality config: missing reality_private_key` | panel 下发 reality 配置缺 private key | 检查 panel 后台 reality 节点配置是否完整 |
| 容器无限 `Restarting (0)` | dummy / 离线 panel 被配成 ApiHost 但 `--restart always` | 用 `yunzes-node fake-test` 替代手动 dummy；或临时 `redeploy --no-restart` |
| `docker logs` 出现 panic | 已通过 C5 修复理论上消除；如再发生，立即 `yunzes-node backup` 留证据，提 issue 附 `docker logs` |
| 升级后业务不通 | PostCheck 应已触发自动回滚；若没回滚，手动 `yunzes-node rollback` 选最近一个 |
| 端口 443 既要 reality 又要 hysteria2 | 一个 tcp 一个 udp 可共存；脚本会接受 |

### 进阶：直接 docker run 或 docker compose

如果你不想用菜单脚本，仓库里有 `docker-compose.yml` 和 `Scripts/docker-run.sh` 两个轻量入口可直接用，行为与菜单脚本一致。

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

