#!/usr/bin/env bash
# yunzes-node management script.
#
# Production entry-point: install once via this script and the global
# `yunzes-node` command becomes the operator's daily driver. Bare invocation
# launches the interactive menu; every menu item also has a non-interactive
# subcommand (`yunzes-node install`, `yunzes-node verify`, `yunzes-node
# fake-test`, ...).
#
# Default deployment is always Docker + host network + single container,
# never split xray/sing-box into separate containers. The Go binary inside
# the image already links both runtimes (C0-C5 refactor).
#
# This script is self-contained — it depends on coreutils, bash 4+, docker,
# jq, curl, tar, and python3 (only for fake-test). Source-tree-only features
# (e.g. `docker build` from local Dockerfile) auto-detect and gracefully
# disable when run from /usr/bin/yunzes-node.

set -uo pipefail
IFS=$'\n\t'

readonly SCRIPT_VERSION="1.0.0"
readonly NAME="yunzes-node"
readonly DEFAULT_IMAGE="yunzes-node:latest"
readonly CONFIG_DIR="/etc/yunzes-node"
readonly CONFIG_FILE="${CONFIG_DIR}/config.json"
readonly CERTS_DIR="${CONFIG_DIR}/certs"
readonly RUN_DIR="/opt/yunzes-node"
readonly BACKUP_DIR="${RUN_DIR}/backups"
readonly LOG_DIR="${RUN_DIR}/logs"
readonly STATE_DIR="${RUN_DIR}/state"
readonly INSTALLED_PATH="/usr/bin/${NAME}"
readonly FAKE_PANEL_FILE="/tmp/fake_panel.py"
readonly FAKE_PANEL_PID_FILE="/tmp/fake_panel.pid"
readonly FAKE_PANEL_LOG_FILE="/tmp/fake_panel.log"
readonly FAKE_PANEL_PORT=9999
readonly FAKE_TEST_CONFIG="/tmp/yunzes-fake-config.json"

readonly C_RED=$'\033[0;31m'
readonly C_GREEN=$'\033[0;32m'
readonly C_YELLOW=$'\033[0;33m'
readonly C_BLUE=$'\033[0;34m'
readonly C_CYAN=$'\033[0;36m'
readonly C_BOLD=$'\033[1m'
readonly C_PLAIN=$'\033[0m'

# Per-precheck status counters; the precheck() function tallies into these.
PRECHECK_PASS=0
PRECHECK_WARN=0
PRECHECK_FAIL=0

# Subcommand-level flags. Reset by parse_flags().
NO_RESTART=0
RESTART_POLICY="always"

# Detected at boot; SOURCE_DIR is non-empty iff a Dockerfile sits next to (or
# above) the script and we can do `docker build` from local sources.
SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null || echo "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(dirname "$SCRIPT_PATH")"
SOURCE_DIR=""

# -----------------------------------------------------------------------------
# Output helpers — all bilingual-aware (operator-facing messages are zh-CN).
# -----------------------------------------------------------------------------
info()    { printf "%s[INFO]%s %s\n" "$C_BLUE"   "$C_PLAIN" "$*"; }
ok()      { printf "%s[ OK ]%s %s\n" "$C_GREEN"  "$C_PLAIN" "$*"; }
warn()    { printf "%s[WARN]%s %s\n" "$C_YELLOW" "$C_PLAIN" "$*" >&2; }
fail()    { printf "%s[FAIL]%s %s\n" "$C_RED"    "$C_PLAIN" "$*" >&2; }
step()    { printf "%s[STEP]%s %s\n" "$C_CYAN"   "$C_PLAIN" "$*"; }
fix_hint(){ printf "%s[FIX ]%s %s\n" "$C_YELLOW" "$C_PLAIN" "$*" >&2; }
err_exit(){ fail "$*"; exit 1; }
banner_line(){ printf "%s%s%s\n" "$C_CYAN" "==============================================" "$C_PLAIN"; }

confirm() {
    # confirm "prompt" "default(y|n)"
    local prompt="$1" default="${2:-n}" reply
    if [[ "$default" == "y" ]]; then
        prompt="$prompt [Y/n]: "
    else
        prompt="$prompt [y/N]: "
    fi
    read -rp "$prompt" reply || return 1
    reply="${reply:-$default}"
    [[ "$reply" =~ ^[Yy]$ ]]
}

confirm_phrase() {
    # confirm_phrase "prompt" "EXACT EXPECTED PHRASE"
    local prompt="$1" expected="$2" reply
    read -rp "$prompt: " reply || return 1
    [[ "$reply" == "$expected" ]]
}

mask_api_key() {
    # mask_api_key "secretvalue1234" -> "secr********1234"
    local k="$1" n=${#1}
    if (( n <= 8 )); then
        printf '****'
    else
        printf '%s********%s' "${k:0:4}" "${k: -4}"
    fi
}

# -----------------------------------------------------------------------------
# Detection helpers
# -----------------------------------------------------------------------------
is_root() { [[ $EUID -eq 0 ]]; }

detect_os() {
    # Echoes one of: debian, ubuntu, other
    if [[ -f /etc/os-release ]]; then
        # shellcheck disable=SC1091
        source /etc/os-release
        case "${ID:-}" in
            debian)   echo debian ;;
            ubuntu)   echo ubuntu ;;
            *)        echo "${ID_LIKE:-other}" | tr ' ' '\n' | grep -E '^(debian|ubuntu)$' | head -1 || echo other ;;
        esac
    else
        echo other
    fi
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *) echo "$(uname -m)" ;;
    esac
}

detect_source_dir() {
    # If the script lives inside a checkout that has Dockerfile + go.mod,
    # echo the path; otherwise echo nothing.
    local d="$SCRIPT_DIR"
    for _ in 1 2; do
        if [[ -f "$d/Dockerfile" && -f "$d/go.mod" ]]; then
            echo "$d"
            return 0
        fi
        d="$(dirname "$d")"
    done
    return 1
}

# Returns:
#   0 — docker reachable as current user
#   10 — docker not installed
#   11 — docker daemon not running
#   12 — current user has no docker socket permission
detect_docker_state() {
    if ! command -v docker >/dev/null 2>&1; then
        return 10
    fi
    local out rc
    out="$(docker ps 2>&1)" && rc=0 || rc=$?
    if [[ $rc -eq 0 ]]; then
        return 0
    fi
    if echo "$out" | grep -qiE 'permission denied|/var/run/docker.sock'; then
        return 12
    fi
    if echo "$out" | grep -qiE 'cannot connect to the docker daemon'; then
        return 11
    fi
    return 12
}

container_exists() {
    docker ps -a --format '{{.Names}}' 2>/dev/null | grep -Fxq "$NAME"
}

container_running() {
    docker ps --format '{{.Names}}' 2>/dev/null | grep -Fxq "$NAME"
}

current_image_id() {
    docker inspect --format '{{.Image}}' "$NAME" 2>/dev/null || true
}

current_image_name() {
    docker inspect --format '{{.Config.Image}}' "$NAME" 2>/dev/null || true
}

# -----------------------------------------------------------------------------
# parse_flags consumes shared flags (--no-restart) from "$@" and re-exports
# the remaining args via the FLAGS_REST array.
# -----------------------------------------------------------------------------
FLAGS_REST=()
parse_flags() {
    NO_RESTART=0
    RESTART_POLICY="always"
    FLAGS_REST=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --no-restart) NO_RESTART=1; RESTART_POLICY="no"; shift ;;
            --) shift; FLAGS_REST+=("$@"); break ;;
            *) FLAGS_REST+=("$1"); shift ;;
        esac
    done
}

# -----------------------------------------------------------------------------
# Banner & menu
# -----------------------------------------------------------------------------
print_banner() {
    cat <<BANNER
${C_CYAN}
   __ __ _   _ _ _____  ___ ___    __ _  ___  ___  ___
   \\ V / | | | | |_  / / __/ __|__/ /| || _ \\| _ \\| __|
    | || |_| |_| |/ /  \\__ \\__ \\__/ /_| || _ /| _ /| _|
    |_||___\\___/|_/___||___/___/_/  __\\__|___||___||___|
${C_PLAIN}
   yunzes-node management v${SCRIPT_VERSION}     image: ${DEFAULT_IMAGE}
   config dir: ${CONFIG_DIR}     run dir: ${RUN_DIR}
BANNER
}

print_menu() {
    cat <<EOF
${C_BOLD}管理菜单${C_PLAIN}

  ${C_GREEN} 1${C_PLAIN}) 安装 yunzes-node
  ${C_GREEN} 2${C_PLAIN}) 升级 yunzes-node
  ${C_GREEN} 3${C_PLAIN}) 启动 yunzes-node
  ${C_GREEN} 4${C_PLAIN}) 停止 yunzes-node
  ${C_GREEN} 5${C_PLAIN}) 重启 yunzes-node
  ${C_GREEN} 6${C_PLAIN}) 重新部署容器
  ${C_GREEN} 7${C_PLAIN}) 查看运行状态
  ${C_GREEN} 8${C_PLAIN}) 查看实时日志
  ${C_GREEN} 9${C_PLAIN}) 查看最近日志
  ${C_GREEN}10${C_PLAIN}) 验证节点服务
  ${C_GREEN}11${C_PLAIN}) 编辑配置文件
  ${C_GREEN}12${C_PLAIN}) 查看当前配置（隐藏 ApiKey）
  ${C_GREEN}13${C_PLAIN}) 生成配置模板
  ${C_GREEN}14${C_PLAIN}) 测试连接 panel server
  ${C_GREEN}15${C_PLAIN}) 查看监听端口
  ${C_GREEN}16${C_PLAIN}) 查看 Docker 容器信息
  ${C_GREEN}17${C_PLAIN}) 备份当前配置
  ${C_GREEN}18${C_PLAIN}) 回滚到上一个备份
  ${C_GREEN}19${C_PLAIN}) 清理旧镜像
  ${C_GREEN}20${C_PLAIN}) 运行 fake panel 四协议验证
  ${C_GREEN}21${C_PLAIN}) 停止 fake panel
  ${C_GREEN}22${C_PLAIN}) 卸载程序，保留配置和证书
  ${C_GREEN}23${C_PLAIN}) ${C_YELLOW}完全卸载（删除配置 + 证书）${C_PLAIN}
  ${C_GREEN}24${C_PLAIN}) 安装/更新命令入口 ${INSTALLED_PATH}
  ${C_GREEN}25${C_PLAIN}) 退出

EOF
}

cmd_menu() {
    while true; do
        clear || true
        print_banner
        print_menu
        local choice
        read -rp "请选择 [1-25]: " choice || break
        echo
        case "$choice" in
            1)  cmd_install ;;
            2)  cmd_update ;;
            3)  cmd_start ;;
            4)  cmd_stop ;;
            5)  cmd_restart ;;
            6)  cmd_redeploy ;;
            7)  cmd_status ;;
            8)  cmd_follow_log ;;
            9)  cmd_logs ;;
            10) cmd_verify ;;
            11) cmd_edit_config ;;
            12) cmd_show_config ;;
            13) cmd_gen_config ;;
            14) cmd_check_panel ;;
            15) cmd_ports ;;
            16) cmd_containers ;;
            17) cmd_backup ;;
            18) cmd_rollback ;;
            19) cmd_cleanup_images ;;
            20) cmd_fake_test ;;
            21) cmd_stop_fake_panel ;;
            22) cmd_uninstall ;;
            23) cmd_uninstall_full ;;
            24) cmd_setup_entry ;;
            25|q|Q|exit) info "再见。"; return 0 ;;
            *)  warn "无效选项：$choice" ;;
        esac
        echo
        read -rp "按回车返回菜单..." _ || true
    done
}

# -----------------------------------------------------------------------------
# PreCheck — 23 items per spec. Each helper records into PRECHECK_PASS/
# WARN/FAIL via _pcok / _pcwarn / _pcfail.
# -----------------------------------------------------------------------------
_pcok()   { ok "$1";   PRECHECK_PASS=$((PRECHECK_PASS+1)); }
_pcwarn() { warn "$1"; PRECHECK_WARN=$((PRECHECK_WARN+1)); }
_pcfail() { fail "$1"; PRECHECK_FAIL=$((PRECHECK_FAIL+1)); }

precheck() {
    PRECHECK_PASS=0; PRECHECK_WARN=0; PRECHECK_FAIL=0

    step "PreCheck 开始"

    # 1. root
    if is_root; then
        _pcok "运行用户：root"
    else
        _pcfail "必须使用 root 运行（当前 UID=$EUID）"
        fix_hint "改用：sudo $SCRIPT_PATH ${1:-menu}"
    fi

    # 2. OS
    local os arch
    os=$(detect_os)
    case "$os" in
        debian|ubuntu) _pcok "操作系统：$os" ;;
        *)             _pcwarn "未在 Debian / Ubuntu 上测试（detected: $os），脚本可能仍能运行" ;;
    esac

    # 3. arch
    arch=$(detect_arch)
    case "$arch" in
        amd64|arm64) _pcok "CPU 架构：$arch" ;;
        *)           _pcwarn "未测试的 CPU 架构：$arch（amd64 / arm64 之外的平台风险自担）" ;;
    esac

    # 4-7. Docker installed / daemon / current-user permission
    detect_docker_state
    case $? in
        0)
            _pcok "Docker 可用且当前用户能调用 docker ps"
            ;;
        10)
            _pcfail "Docker 未安装"
            fix_hint "Debian/Ubuntu 安装：apt update && apt install -y docker.io"
            ;;
        11)
            _pcfail "Docker daemon 未运行"
            fix_hint "启动 daemon：service docker start  或  systemctl start docker"
            ;;
        12)
            _pcfail "当前用户无 /var/run/docker.sock 权限"
            fix_hint "永久路：usermod -aG docker \$USER  然后重新登录终端"
            fix_hint "临时路：用 sudo 重跑此脚本，或切到 root"
            ;;
    esac

    # 8. docker compose (informational)
    if docker compose version >/dev/null 2>&1; then
        _pcok "docker compose 可用（v2 plugin）"
    elif command -v docker-compose >/dev/null 2>&1; then
        _pcok "docker-compose 可用（v1 binary）"
    else
        _pcwarn "未发现 docker compose（不影响一键脚本，docker run 直跑即可）"
    fi

    # 9-13. CLI deps
    local tool
    for tool in curl jq tar git ss; do
        if command -v "$tool" >/dev/null 2>&1; then
            _pcok "命令存在：$tool"
        else
            case "$tool" in
                ss) _pcwarn "缺少 $tool（端口检查会降级）；apt install -y iproute2" ;;
                git) _pcwarn "缺少 $tool（仅源码升级时需要）；apt install -y git" ;;
                jq)  _pcfail "缺少 $tool（必需，用于校验和编辑 config.json）；apt install -y jq" ;;
                *)   _pcfail "缺少 $tool；apt install -y $tool" ;;
            esac
        fi
    done

    # 14-17. dirs
    local d
    for d in "$CONFIG_DIR" "$CERTS_DIR" "$BACKUP_DIR"; do
        if [[ -d "$d" ]]; then
            _pcok "目录存在：$d"
        else
            _pcwarn "目录不存在：$d（安装流程会自动创建）"
        fi
    done
    if [[ -f "$CONFIG_FILE" ]]; then
        if jq empty "$CONFIG_FILE" 2>/dev/null; then
            _pcok "config.json 存在且 JSON 合法"
        else
            _pcfail "config.json 存在但非合法 JSON：$CONFIG_FILE"
            fix_hint "用菜单 11 / yunzes-node edit-config 修复"
        fi
    else
        _pcwarn "config.json 不存在：$CONFIG_FILE（安装流程会引导生成）"
    fi

    # 18-19. existing container / image
    if container_exists; then
        _pcok "已有容器 $NAME（升级 / 重新部署可继续）"
    else
        _pcwarn "尚无容器 $NAME（首次安装）"
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        _pcok "已有镜像 $DEFAULT_IMAGE"
    else
        _pcwarn "镜像 $DEFAULT_IMAGE 不存在（安装流程会引导构建或拉取）"
    fi

    # 20-21. ports 80 / 443
    _check_port_free 80 tcp || true
    _check_port_free 443 tcp || true

    # 22. ports from config
    if [[ -f "$CONFIG_FILE" ]] && jq empty "$CONFIG_FILE" 2>/dev/null; then
        local tmp
        tmp=$(mktemp)
        list_config_listen_specs > "$tmp" || true
        local spec
        while read -r spec; do
            [[ -z "$spec" ]] && continue
            local addr port proto
            IFS=':/' read -r addr port proto <<<"$spec"
            _check_port_free "$port" "$proto" || true
        done < "$tmp"
        rm -f "$tmp"
    fi

    # 23. firewall hint + system info
    if command -v ufw >/dev/null 2>&1; then
        local ufw_state
        ufw_state="$(ufw status 2>/dev/null | head -1 || true)"
        _pcwarn "检测到 ufw（$ufw_state）；请确保业务端口已放行，本脚本不主动改动防火墙"
    elif command -v firewall-cmd >/dev/null 2>&1; then
        _pcwarn "检测到 firewalld；请确保业务端口已放行，本脚本不主动改动防火墙"
    else
        _pcok "未检测到 ufw / firewalld 防火墙工具"
    fi
    if command -v free >/dev/null 2>&1; then
        local mem_free
        mem_free=$(free -h --si 2>/dev/null | awk '/^Mem:/{print $7}')
        if [[ -n "$mem_free" ]]; then
            _pcok "可用内存：$mem_free"
        fi
    fi
    local disk_free
    disk_free=$(df -h "$RUN_DIR" 2>/dev/null | awk 'NR==2{print $4}')
    if [[ -n "$disk_free" ]]; then
        _pcok "$RUN_DIR 可用磁盘：$disk_free"
    fi

    echo
    info "PreCheck 汇总：${C_GREEN}PASS=$PRECHECK_PASS${C_PLAIN}  ${C_YELLOW}WARN=$PRECHECK_WARN${C_PLAIN}  ${C_RED}FAIL=$PRECHECK_FAIL${C_PLAIN}"
    if (( PRECHECK_FAIL > 0 )); then
        fail "PreCheck 不通过，安装/升级流程中断。修完上面的 [FAIL] 项再重试。"
        return 1
    fi
    return 0
}

_check_port_free() {
    # _check_port_free PORT TRANSPORT
    local port="$1" proto="$2"
    if ! command -v ss >/dev/null 2>&1; then
        return 0
    fi
    local hits owner
    case "$proto" in
        tcp) hits=$(ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
        udp) hits=$(ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
    esac
    if [[ -z "$hits" ]]; then
        return 0
    fi
    owner=$(echo "$hits" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
    if [[ "$owner" == "$NAME" ]]; then
        _pcok "$port/$proto 已由 yunzes-node 自身监听（属正常）"
    else
        _pcwarn "$port/$proto 已被 $owner 占用；ACME / 节点端口可能冲突"
    fi
}

# -----------------------------------------------------------------------------
# Config helpers — list listen specs, validate listener uniqueness, etc.
# Output of list_config_listen_specs: one "addr:port/proto" per line.
# Uses port_registry rules: "" → 0.0.0.0, ss tcp+udp dual-listing.
# -----------------------------------------------------------------------------
list_config_listen_specs() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        return 0
    fi
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        return 1
    fi
    local idx total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE" 2>/dev/null)
    [[ -z "$total" ]] && total=0
    for ((idx=0; idx<total; idx++)); do
        local node_type listen_ip port
        node_type=$(jq -r ".Nodes[$idx].NodeType // empty" "$CONFIG_FILE")
        listen_ip=$(jq -r ".Nodes[$idx].Options.ListenIP // .Nodes[$idx].ListenIP // \"0.0.0.0\"" "$CONFIG_FILE")
        [[ -z "$listen_ip" || "$listen_ip" == "null" ]] && listen_ip="0.0.0.0"
        # The actual port is panel-driven so we can't know it from local config
        # alone. Instead emit a "?-port" placeholder; verify() and PreCheck
        # only consult this for the address portion.
        case "$node_type" in
            shadowsocks) printf '%s:?/tcp\n%s:?/udp\n' "$listen_ip" "$listen_ip" ;;
            hysteria|hysteria2|tuic) printf '%s:?/udp\n' "$listen_ip" ;;
            vless|vmess|trojan|anytls) printf '%s:?/tcp\n' "$listen_ip" ;;
        esac
    done
}

# -----------------------------------------------------------------------------
# Backup / Restore
# -----------------------------------------------------------------------------
backup_now() {
    # Echoes the backup directory path on success.
    mkdir -p "$BACKUP_DIR"
    local ts dir
    ts=$(date +%Y%m%d-%H%M%S)
    dir="$BACKUP_DIR/$ts"
    mkdir -p "$dir"
    if [[ -f "$CONFIG_FILE" ]]; then
        cp -p "$CONFIG_FILE" "$dir/config.json"
    fi
    if [[ -d "$CERTS_DIR" ]]; then
        # tar avoids permission churn from cp -r on cross-fs setups.
        tar -C "$CONFIG_DIR" -czf "$dir/certs.tar.gz" certs 2>/dev/null || true
    fi
    if container_exists; then
        docker inspect "$NAME" > "$dir/docker-inspect.json" 2>/dev/null || true
        current_image_id   > "$dir/image-id.txt"   2>/dev/null || true
        current_image_name > "$dir/image-name.txt" 2>/dev/null || true
    fi
    echo "$SCRIPT_VERSION" > "$dir/script-version.txt"
    if [[ -n "$SOURCE_DIR" && -d "$SOURCE_DIR/.git" ]]; then
        ( cd "$SOURCE_DIR" && git rev-parse HEAD 2>/dev/null > "$dir/git-commit.txt" ) || true
    fi
    chmod -R go-rwx "$dir" 2>/dev/null || true
    echo "$dir"
}

list_backups() {
    [[ -d "$BACKUP_DIR" ]] || return 0
    find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort -r
}

restore_from_backup() {
    # restore_from_backup BACKUP_DIR_NAME
    local name="$1" dir="$BACKUP_DIR/$1"
    [[ -d "$dir" ]] || { fail "备份目录不存在：$dir"; return 1; }
    step "回滚到备份：$name"
    if [[ -f "$dir/config.json" ]]; then
        cp -p "$dir/config.json" "$CONFIG_FILE"
        ok "config.json 恢复"
    fi
    if [[ -f "$dir/certs.tar.gz" ]]; then
        tar -C "$CONFIG_DIR" -xzf "$dir/certs.tar.gz" 2>/dev/null || warn "证书包解压失败（可手动 tar -xzvf 查看）"
        ok "certs/ 恢复"
    fi
    local image_to_use=""
    if [[ -f "$dir/image-name.txt" ]]; then
        image_to_use="$(<"$dir/image-name.txt")"
    fi
    if [[ -z "$image_to_use" ]]; then
        image_to_use="$DEFAULT_IMAGE"
    fi
    info "使用镜像：$image_to_use"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if ! _docker_run "$image_to_use" "always"; then
        fail "回滚启动容器失败"
        return 1
    fi
    return 0
}

# -----------------------------------------------------------------------------
# Container ops — _docker_run encapsulates the canonical run line.
# -----------------------------------------------------------------------------
_docker_run() {
    # _docker_run IMAGE RESTART_POLICY [extra-args ...]
    local image="$1" restart="${2:-always}"; shift 2 || true
    docker run -d \
        --name "$NAME" \
        --network host \
        --restart "$restart" \
        -v "$CONFIG_DIR:/etc/yunzes-node" \
        "$@" \
        "$image" >/dev/null
}

ensure_dirs() {
    mkdir -p "$CONFIG_DIR" "$CERTS_DIR" "$RUN_DIR" "$BACKUP_DIR" "$LOG_DIR" "$STATE_DIR"
    chmod 700 "$BACKUP_DIR" "$STATE_DIR" 2>/dev/null || true
    chmod 750 "$CERTS_DIR"  2>/dev/null || true
}

# -----------------------------------------------------------------------------
# Verify — three tiers per spec.
# -----------------------------------------------------------------------------
verify_basic() {
    local pass=0 fail=0
    step "Verify L1：基础"
    if container_running; then
        ok "容器 $NAME 运行中"; pass=$((pass+1))
    else
        fail "容器 $NAME 未运行"; fail=$((fail+1))
    fi
    if [[ -f "$CONFIG_FILE" ]]; then
        if jq empty "$CONFIG_FILE" 2>/dev/null; then
            ok "config.json 存在且合法"; pass=$((pass+1))
        else
            fail "config.json 存在但非合法 JSON"; fail=$((fail+1))
        fi
    else
        fail "config.json 不存在"; fail=$((fail+1))
    fi
    if [[ -d "$CERTS_DIR" ]]; then
        ok "certs/ 存在"; pass=$((pass+1))
    else
        warn "certs/ 不存在（仅 cleartext / reality 节点可接受）"
    fi
    local bad
    bad=$(docker logs "$NAME" 2>&1 | grep -E -i 'panic|runtime error|nil pointer dereference|segmentation violation|fatal error' | head -5 || true)
    if [[ -z "$bad" ]]; then
        ok "docker logs 无 panic / fatal / runtime error"; pass=$((pass+1))
    else
        fail "docker logs 出现 panic / fatal / runtime error："
        echo "$bad" | sed 's/^/      /'
        fail=$((fail+1))
    fi
    info "L1 结果：${C_GREEN}PASS=$pass${C_PLAIN}  ${C_RED}FAIL=$fail${C_PLAIN}"
    return $fail
}

verify_network() {
    local pass=0 fail=0 warn=0
    step "Verify L2：网络"
    local mode
    mode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$NAME" 2>/dev/null || true)
    if [[ "$mode" == "host" ]]; then
        ok "容器使用 host network"; pass=$((pass+1))
    elif [[ -z "$mode" ]]; then
        warn "无法读取容器网络模式（容器可能不存在）"; warn=$((warn+1))
    else
        warn "容器使用 $mode 而非 host network（生产建议 host）"; warn=$((warn+1))
    fi
    if [[ ! -f "$CONFIG_FILE" ]]; then
        info "L2 结果：${C_GREEN}PASS=$pass${C_PLAIN}  ${C_YELLOW}WARN=$warn${C_PLAIN}  ${C_RED}FAIL=$fail${C_PLAIN}"
        return $fail
    fi
    # ACME HTTP-01 hint
    if command -v ss >/dev/null 2>&1; then
        local hits owner80
        hits=$(ss -lntp 2>/dev/null | awk '$4 ~ /:80$/' || true)
        if [[ -n "$hits" ]]; then
            owner80=$(echo "$hits" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
            if [[ "$owner80" == "$NAME" ]]; then
                ok "80/tcp 由 yunzes-node 自身监听"
            else
                warn "80/tcp 被 $owner80 占用（若用 ACME HTTP-01 会失败）"; warn=$((warn+1))
            fi
        fi
    fi
    # Per-node listen check using the inferred specs
    local total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE" 2>/dev/null)
    [[ -z "$total" ]] && total=0
    local idx
    for ((idx=0; idx<total; idx++)); do
        local nt
        nt=$(jq -r ".Nodes[$idx].NodeType // empty" "$CONFIG_FILE")
        case "$nt" in
            shadowsocks)
                ok "节点 #$idx ($nt) 协议同时占用 tcp + udp（C2 双登记）"
                ;;
            hysteria|hysteria2|tuic)
                ok "节点 #$idx ($nt) 占用 udp（实际端口由 panel 下发）"
                ;;
            vless|vmess|trojan|anytls)
                ok "节点 #$idx ($nt) 占用 tcp"
                ;;
            *)  warn "节点 #$idx 协议 $nt 未在端口规则表中" ;;
        esac
    done
    info "L2 结果：${C_GREEN}PASS=$pass${C_PLAIN}  ${C_YELLOW}WARN=$warn${C_PLAIN}  ${C_RED}FAIL=$fail${C_PLAIN}"
    return $fail
}

verify_business() {
    local pass=0 fail=0 warn=0
    step "Verify L3：业务"
    if [[ -f "$CONFIG_FILE" ]] && jq empty "$CONFIG_FILE" 2>/dev/null; then
        local hosts
        mapfile -t hosts < <(jq -r '.Nodes[]?.ApiHost // empty' "$CONFIG_FILE" | sort -u)
        local h
        for h in "${hosts[@]}"; do
            [[ -z "$h" ]] && continue
            local code
            code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$h" || true)
            if [[ "$code" =~ ^[2345] ]]; then
                ok "panel 可达：$h  HTTP $code"; pass=$((pass+1))
            else
                warn "panel 无响应：$h（curl 返回 $code）"; warn=$((warn+1))
            fi
        done
    else
        warn "无可用 config.json，跳过 panel 探活"; warn=$((warn+1))
    fi
    local logs
    logs="$(docker logs --tail 500 "$NAME" 2>&1 || true)"
    local marker
    for marker in "Start yunzes-node" "Core Selector" "Adding node inbound" "logical_tag" "core=" "runtime_key" "protocol=" "server_id" "port="; do
        if echo "$logs" | grep -qF "$marker"; then
            ok "日志含字段：$marker"; pass=$((pass+1))
        else
            warn "日志未见：$marker"; warn=$((warn+1))
        fi
    done
    info "L3 结果：${C_GREEN}PASS=$pass${C_PLAIN}  ${C_YELLOW}WARN=$warn${C_PLAIN}  ${C_RED}FAIL=$fail${C_PLAIN}"
    return $fail
}

cmd_verify() {
    verify_basic;    local r1=$?
    echo
    verify_network;  local r2=$?
    echo
    verify_business; local r3=$?
    echo
    if (( r1 + r2 + r3 == 0 )); then
        ok "全部 verify 等级通过"
        return 0
    fi
    fail "verify 有 FAIL 项；查看上方输出"
    return 1
}

# -----------------------------------------------------------------------------
# Install / Update / Lifecycle
# -----------------------------------------------------------------------------
cmd_install() {
    parse_flags "$@"
    if ! precheck install; then return 1; fi
    ensure_dirs

    if [[ -f "$CONFIG_FILE" ]]; then
        echo "已检测到 $CONFIG_FILE"
        echo "  1) 使用现有配置"
        echo "  2) 备份后重新生成"
        echo "  3) 退出"
        local ans
        read -rp "请选择 [1-3]: " ans
        case "$ans" in
            1) info "使用现有配置" ;;
            2)
                local b
                b=$(backup_now)
                ok "已备份到 $b"
                gen_config_template "$CONFIG_FILE"
                if confirm "是否进入交互式生成（推荐）" y; then
                    gen_config_interactive
                fi
                ;;
            3) info "用户取消"; return 0 ;;
            *) warn "无效选项，使用现有配置" ;;
        esac
    else
        info "config.json 不存在，先生成模板"
        gen_config_template "$CONFIG_FILE.example"
        if confirm "是否进入交互式生成 $CONFIG_FILE" y; then
            gen_config_interactive
        else
            warn "请先 cp $CONFIG_FILE.example $CONFIG_FILE 并按需修改，再重跑安装"
            return 0
        fi
    fi

    # Image source
    if ! docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        echo "镜像 $DEFAULT_IMAGE 不存在。镜像来源："
        echo "  1) 本地源码 docker build（仅当从源码目录运行此脚本时可用）"
        echo "  2) 拉取远程镜像"
        echo "  3) 手动输入镜像名"
        local choice
        read -rp "请选择 [1-3]: " choice
        case "$choice" in
            1)
                if [[ -z "$SOURCE_DIR" ]]; then
                    fail "未在源码目录运行；改用 2 或 3"
                    return 1
                fi
                step "在 $SOURCE_DIR 执行 docker build"
                ( cd "$SOURCE_DIR" && docker build -t "$DEFAULT_IMAGE" . ) || { fail "docker build 失败"; return 1; }
                ok "镜像构建完成：$DEFAULT_IMAGE"
                ;;
            2)
                read -rp "拉取的镜像名 [默认 $DEFAULT_IMAGE]: " img
                img="${img:-$DEFAULT_IMAGE}"
                docker pull "$img" || { fail "docker pull 失败"; return 1; }
                docker tag "$img" "$DEFAULT_IMAGE"
                ok "已 tag 为 $DEFAULT_IMAGE"
                ;;
            3)
                read -rp "镜像名: " img
                [[ -z "$img" ]] && { fail "镜像名为空"; return 1; }
                docker tag "$img" "$DEFAULT_IMAGE" || { fail "tag 失败"; return 1; }
                ;;
            *) fail "无效选项"; return 1 ;;
        esac
    fi

    # Backup before any container churn
    local b
    b=$(backup_now)
    info "Pre-install 备份：$b"

    # Start
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    step "启动容器（restart=$RESTART_POLICY）"
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        fail "docker run 失败"
        return 1
    fi
    ok "容器已启动：$NAME"
    sleep 3
    cmd_verify || warn "verify 有未通过项，请查看上方输出"
}

cmd_update() {
    parse_flags "$@"
    if ! precheck update; then return 1; fi

    if ! container_exists; then
        warn "容器不存在；切换到 install 流程"
        cmd_install "$@"
        return $?
    fi

    local backup_dir
    backup_dir=$(backup_now)
    ok "升级前已备份：$backup_dir"

    local previous_image
    previous_image=$(current_image_id)

    # Image source
    echo "升级镜像来源："
    echo "  1) 本地源码 docker build（推荐 — 与你 commit 完全对齐）"
    echo "  2) 拉取远程镜像"
    echo "  3) 跳过镜像更新（仅重新创建容器）"
    local choice
    read -rp "请选择 [1-3]: " choice
    case "$choice" in
        1)
            [[ -z "$SOURCE_DIR" ]] && { fail "未在源码目录运行"; return 1; }
            step "docker build"
            ( cd "$SOURCE_DIR" && docker build -t "$DEFAULT_IMAGE" . ) || { fail "build 失败"; return 1; }
            ;;
        2)
            read -rp "镜像名 [默认 $DEFAULT_IMAGE]: " img
            img="${img:-$DEFAULT_IMAGE}"
            docker pull "$img" || { fail "pull 失败"; return 1; }
            docker tag "$img" "$DEFAULT_IMAGE"
            ;;
        3) info "跳过镜像更新" ;;
        *) fail "无效选项"; return 1 ;;
    esac

    step "停止旧容器"
    docker stop "$NAME" >/dev/null 2>&1 || true
    docker rm "$NAME"   >/dev/null 2>&1 || true

    step "启动新容器（restart=$RESTART_POLICY）"
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        warn "新容器启动失败，触发自动回滚"
        _auto_rollback "$backup_dir" "$previous_image"
        return 1
    fi
    sleep 4
    if ! verify_basic; then
        warn "verify L1 失败，触发自动回滚"
        _auto_rollback "$backup_dir" "$previous_image"
        return 1
    fi
    ok "升级完成"
    cmd_verify || warn "verify 有未通过项，请查看上方输出"
}

_auto_rollback() {
    # _auto_rollback BACKUP_DIR PREVIOUS_IMAGE_ID_OR_NAME
    local backup_dir="$1" prev="$2"
    step "Auto rollback → $backup_dir"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if [[ -f "$backup_dir/config.json" ]]; then
        cp -p "$backup_dir/config.json" "$CONFIG_FILE"
    fi
    if [[ -f "$backup_dir/certs.tar.gz" ]]; then
        tar -C "$CONFIG_DIR" -xzf "$backup_dir/certs.tar.gz" 2>/dev/null || true
    fi
    local image_to_use="$prev"
    if [[ -z "$image_to_use" || "$image_to_use" == "<no value>" ]]; then
        if [[ -f "$backup_dir/image-name.txt" ]]; then
            image_to_use="$(<"$backup_dir/image-name.txt")"
        fi
    fi
    [[ -z "$image_to_use" ]] && image_to_use="$DEFAULT_IMAGE"
    info "rollback image: $image_to_use"
    if _docker_run "$image_to_use" "always"; then
        sleep 3
        if verify_basic; then
            ok "回滚成功"
            return 0
        fi
    fi
    fail "回滚启动失败 — 容器状态请用 yunzes-node status / docker logs $NAME 查看"
    return 1
}

cmd_start() {
    if container_exists; then
        if container_running; then
            info "$NAME 已经在运行"
        else
            docker start "$NAME" >/dev/null && ok "已启动" || fail "启动失败"
        fi
    else
        warn "容器不存在；执行 yunzes-node install 先安装"
    fi
}

cmd_stop() {
    if container_running; then
        docker stop "$NAME" >/dev/null && ok "已停止"
    else
        info "$NAME 未在运行"
    fi
}

cmd_restart() {
    if container_exists; then
        docker restart "$NAME" >/dev/null && ok "已重启"
    else
        warn "容器不存在"
    fi
}

cmd_redeploy() {
    parse_flags "$@"
    if ! container_exists; then
        warn "容器不存在；切换到 install"
        cmd_install "$@"
        return $?
    fi
    local backup_dir
    backup_dir=$(backup_now)
    ok "重新部署前备份：$backup_dir"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        fail "启动失败，自动回滚"
        _auto_rollback "$backup_dir" "$DEFAULT_IMAGE"
        return 1
    fi
    sleep 3
    cmd_verify || warn "verify 有未通过项"
}

cmd_status() {
    if ! container_exists; then
        info "$NAME 容器不存在"
        return 0
    fi
    docker ps -a --filter "name=^${NAME}$" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Ports}}"
    echo
    docker stats --no-stream "$NAME" 2>/dev/null || true
}

cmd_logs() {
    local n="${1:-100}"
    case "$n" in
        100|300) ;;
        ''|*[!0-9]*) n=100 ;;
    esac
    docker logs --tail "$n" "$NAME" 2>&1 || warn "容器不存在"
}

cmd_follow_log() {
    docker logs -f "$NAME" 2>&1 || warn "容器不存在"
}

cmd_edit_config() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        warn "$CONFIG_FILE 不存在；先用 yunzes-node gen-config 生成"
        return 1
    fi
    local editor="${EDITOR:-}"
    if [[ -z "$editor" ]]; then
        if   command -v nano >/dev/null 2>&1; then editor=nano
        elif command -v vi   >/dev/null 2>&1; then editor=vi
        else fail "未找到 nano/vi，请设置 EDITOR 环境变量"; return 1
        fi
    fi
    local backup
    backup=$(backup_now)
    info "已备份当前配置：$backup"
    cp -p "$CONFIG_FILE" "${CONFIG_FILE}.bak"
    "$editor" "$CONFIG_FILE"
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        fail "保存的 config.json 非合法 JSON；恢复 .bak"
        mv "${CONFIG_FILE}.bak" "$CONFIG_FILE"
        return 1
    fi
    rm -f "${CONFIG_FILE}.bak"
    ok "JSON 校验通过"
    if container_running && confirm "立即重启容器使配置生效" y; then
        cmd_restart
    fi
}

cmd_show_config() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        warn "$CONFIG_FILE 不存在"
        return 1
    fi
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        fail "config.json 非合法 JSON"; return 1
    fi
    # Mask ApiKey across every node
    jq '
        .Nodes |= (map(
            if has("ApiKey") and (.ApiKey | type) == "string" and (.ApiKey | length) > 0 then
                .ApiKey = (.ApiKey[0:4] + "********" + .ApiKey[-4:])
            else . end
        ))
    ' "$CONFIG_FILE"
}

gen_config_template() {
    # gen_config_template DEST_PATH
    local dest="$1"
    cat > "$dest" <<'JSON'
{
    "Log": {"Level": "info"},
    "Cores": [
        {"Type": "xray"},
        {"Type": "sing"}
    ],
    "Nodes": [
        {
            "ApiHost": "https://your-panel.example.com",
            "ApiKey": "REPLACE_WITH_REAL_SECRET",
            "NodeID": 1,
            "NodeType": "vless",
            "Timeout": 30,
            "ListenIP": "0.0.0.0",
            "CertConfig": {
                "CertMode": "http",
                "CertDomain": "node.example.com",
                "CertFile":   "/etc/yunzes-node/certs/vless1.crt",
                "KeyFile":    "/etc/yunzes-node/certs/vless1.key",
                "Email":      "admin@example.com",
                "Provider":   "",
                "RenewBeforeDays": 30
            }
        }
    ]
}
JSON
    ok "已写入模板：$dest"
}

cmd_gen_config() {
    ensure_dirs
    if [[ -f "$CONFIG_FILE" ]] && ! confirm "$CONFIG_FILE 已存在，是否覆盖（会先备份）" n; then
        return 0
    fi
    [[ -f "$CONFIG_FILE" ]] && backup_now >/dev/null
    gen_config_template "$CONFIG_FILE"
    if confirm "进入交互生成多节点配置" y; then
        gen_config_interactive
    fi
}

gen_config_interactive() {
    info "进入交互式配置生成 — 按 Ctrl-C 中途退出会保留模板状态"
    echo
    local nodes_json="[]"
    while true; do
        echo "${C_BOLD}--- 添加节点 #$(echo "$nodes_json" | jq 'length') ---${C_PLAIN}"
        local api_host api_key node_id node_type listen_ip timeout
        local cert_mode cert_domain cert_file key_file email
        read -rp "ApiHost [https://your-panel.example.com]: " api_host
        api_host="${api_host:-https://your-panel.example.com}"
        read -rp "ApiKey: " api_key
        read -rp "NodeID (整数) [1]: " node_id
        node_id="${node_id:-1}"
        echo "支持协议: vless / vmess / trojan / shadowsocks / hysteria2 / tuic / anytls"
        read -rp "NodeType [vless]: " node_type
        node_type="${node_type:-vless}"
        read -rp "ListenIP [0.0.0.0]: " listen_ip
        listen_ip="${listen_ip:-0.0.0.0}"
        read -rp "Timeout (秒) [30]: " timeout
        timeout="${timeout:-30}"

        local need_tls=0
        case "$node_type" in
            hysteria2|tuic|anytls) need_tls=1 ;;
            vless|vmess|trojan)
                if confirm "该协议是否启用 TLS（reality / 无加密回答 N）" y; then
                    need_tls=1
                fi
                ;;
        esac

        local cert_obj="null"
        if (( need_tls )); then
            echo "CertMode 选项: http (ACME HTTP-01) / dns (ACME DNS-01) / file (你提供) / self (自签) / none"
            read -rp "CertMode [self]: " cert_mode
            cert_mode="${cert_mode:-self}"
            case "$cert_mode" in
                http|dns|file|self)
                    read -rp "CertDomain: " cert_domain
                    read -rp "CertFile [/etc/yunzes-node/certs/${node_type}${node_id}.crt]: " cert_file
                    cert_file="${cert_file:-/etc/yunzes-node/certs/${node_type}${node_id}.crt}"
                    read -rp "KeyFile  [/etc/yunzes-node/certs/${node_type}${node_id}.key]: " key_file
                    key_file="${key_file:-/etc/yunzes-node/certs/${node_type}${node_id}.key}"
                    if [[ "$cert_mode" =~ ^(http|dns)$ ]]; then
                        read -rp "Email (ACME 注册用): " email
                    else
                        email=""
                    fi
                    cert_obj=$(jq -n \
                        --arg m "$cert_mode" --arg d "$cert_domain" \
                        --arg cf "$cert_file" --arg kf "$key_file" --arg e "$email" \
                        '{CertMode:$m, CertDomain:$d, CertFile:$cf, KeyFile:$kf, Email:$e, RenewBeforeDays:30}')
                    ;;
                none|"") cert_obj='{"CertMode":"none"}' ;;
                *) warn "未知 CertMode '$cert_mode'，回退到 none"; cert_obj='{"CertMode":"none"}' ;;
            esac
        else
            cert_obj='{"CertMode":"none"}'
        fi

        local node_json
        node_json=$(jq -n \
            --arg ah "$api_host" --arg ak "$api_key" \
            --argjson nid "$node_id" --arg nt "$node_type" \
            --argjson tm "$timeout" --arg lip "$listen_ip" \
            --argjson cert "$cert_obj" \
            '{ApiHost:$ah, ApiKey:$ak, NodeID:$nid, NodeType:$nt, Timeout:$tm, ListenIP:$lip, CertConfig:$cert}')
        nodes_json=$(echo "$nodes_json" | jq --argjson n "$node_json" '. + [$n]')
        echo
        if ! confirm "继续添加下一个节点" n; then
            break
        fi
    done

    local final
    final=$(jq -n --argjson nodes "$nodes_json" '{
        Log: {Level: "info"},
        Cores: [{Type:"xray"}, {Type:"sing"}],
        Nodes: $nodes
    }')
    if echo "$final" | jq empty 2>/dev/null; then
        echo "$final" > "$CONFIG_FILE"
        chmod 600 "$CONFIG_FILE" 2>/dev/null || true
        ok "已写入 $CONFIG_FILE"
    else
        fail "生成的 JSON 不合法（脚本 bug，请反馈）"
        return 1
    fi
}

cmd_check_panel() {
    [[ -f "$CONFIG_FILE" ]] || { fail "无 config.json"; return 1; }
    jq empty "$CONFIG_FILE" 2>/dev/null || { fail "config.json 非合法 JSON"; return 1; }
    local total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE")
    local idx
    for ((idx=0; idx<total; idx++)); do
        local host node_id ntype path
        host=$(jq -r ".Nodes[$idx].ApiHost"  "$CONFIG_FILE")
        node_id=$(jq -r ".Nodes[$idx].NodeID" "$CONFIG_FILE")
        ntype=$(jq -r  ".Nodes[$idx].NodeType" "$CONFIG_FILE")
        path="${host%/}/v1/server/config?protocol=${ntype}&server_id=${node_id}"
        step "节点 #$idx → $path"
        local code
        code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$path" || true)
        case "$code" in
            200) ok "HTTP 200 — panel 返回节点配置" ;;
            401|403) warn "HTTP $code — panel 拒绝（ApiKey 可能错或权限不足）" ;;
            404) warn "HTTP 404 — 节点 ID $node_id 不存在或路径不对" ;;
            000|"") fail "无法连接 $host" ;;
            *) info "HTTP $code（具体含义看 panel 文档）" ;;
        esac
    done
}

cmd_ports() {
    if ! command -v ss >/dev/null 2>&1; then
        fail "缺少 ss 命令；apt install -y iproute2"
        return 1
    fi
    if container_running; then
        local pid
        pid=$(docker inspect --format '{{.State.Pid}}' "$NAME" 2>/dev/null || true)
        info "yunzes-node PID = $pid（host network 下与宿主端口表合并）"
        ss -lntup | awk -v p="$pid" 'NR==1 || $0 ~ p'
    else
        warn "容器未运行 — 显示宿主当前所有监听"
        ss -lntup
    fi
}

cmd_containers() {
    docker ps -a --filter "name=^${NAME}$" --no-trunc
    echo
    info "容器详细信息："
    docker inspect "$NAME" 2>/dev/null | head -80 || warn "容器不存在"
}

cmd_backup() {
    ensure_dirs
    local b
    b=$(backup_now)
    ok "备份完成：$b"
    ls -lh "$b"
    echo
    info "回滚命令：yunzes-node rollback   或菜单 18"
}

cmd_rollback() {
    local backups
    mapfile -t backups < <(list_backups)
    if (( ${#backups[@]} == 0 )); then
        warn "没有可用的备份"
        return 0
    fi
    echo "可回滚的备份："
    local i
    for ((i=0; i<${#backups[@]}; i++)); do
        echo "  $((i+1))) ${backups[$i]}"
    done
    echo "  q) 取消"
    local choice
    read -rp "选择 [1-${#backups[@]}, q]: " choice
    [[ "$choice" =~ ^[Qq]$ ]] && { info "已取消"; return 0; }
    if ! [[ "$choice" =~ ^[0-9]+$ ]] || (( choice < 1 || choice > ${#backups[@]} )); then
        fail "无效选项"; return 1
    fi
    local target="${backups[$((choice-1))]}"
    if ! confirm "回滚到 $target  这将覆盖当前 config.json + certs/ + 容器" n; then
        info "已取消"; return 0
    fi
    local before
    before=$(backup_now)
    info "当前状态已备份到 $before（rollback-before-保险）"
    if restore_from_backup "$target"; then
        sleep 3
        cmd_verify || warn "verify 有未通过项"
    else
        fail "回滚失败 — yunzes-node status 查看容器；$before 是回滚前的备份"
        return 1
    fi
}

cmd_cleanup_images() {
    info "悬空镜像（dangling）："
    docker images --filter "dangling=true" --format "table {{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}"
    echo
    if confirm "清理所有 dangling 镜像" n; then
        docker image prune -f
        ok "清理完成"
    fi
    echo
    info "全部 yunzes-node 历史镜像（含 untagged）："
    docker images --filter "reference=yunzes-node" --format "table {{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}"
}

# -----------------------------------------------------------------------------
# Fake panel 4-protocol test
# -----------------------------------------------------------------------------
write_fake_panel_py() {
    cat > "$FAKE_PANEL_FILE" <<'PYEOF'
#!/usr/bin/env python3
"""Fake panel for yunzes-node integration testing.

Listens on 127.0.0.1:9999. Dispatches by (protocol, server_id) so a single
panel instance can drive 4 protocols out of one container's NodeConfig list.
"""
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs

USERS = {"users": [
    {"id": 1, "uuid": "11111111-1111-1111-1111-111111111111", "speed_limit": 0, "device_limit": 0},
    {"id": 2, "uuid": "22222222-2222-2222-2222-222222222222", "speed_limit": 0, "device_limit": 0},
]}
ALIVE = {"alive": {}}
BASIC = {"push_interval": 30, "pull_interval": 60}

NODE_TABLE = {
    ("vless", "101"): {"basic": BASIC, "protocol": "vless", "config": {
        "port": 8101, "transport": "tcp", "security": "tls",
        "security_config": {"sni": "vless.test"},
    }},
    ("shadowsocks", "102"): {"basic": BASIC, "protocol": "shadowsocks", "config": {
        "port": 8102, "method": "aes-256-gcm",
    }},
    ("hysteria2", "103"): {"basic": BASIC, "protocol": "hysteria2", "config": {
        "port": 8103, "up_mbps": 100, "down_mbps": 100, "obfs_password": "obfs-secret",
        "security_config": {"sni": "hy2.test"},
    }},
    ("vless", "104"): {"basic": BASIC, "protocol": "vless", "config": {
        "port": 8104, "transport": "tcp", "security": "reality",
        "security_config": {
            "sni": "reality.test",
            "reality_server_addr": "www.cloudflare.com",
            "reality_server_port": 443,
            "reality_private_key": "wEbNI8QwM1XLgX-ucy7Qwp6msGmGCfSMQClC-VRjV3w",
            "reality_short_id": "0123456789abcdef",
        },
    }},
}

class Handler(BaseHTTPRequestHandler):
    def _send_json(self, body, status=200):
        b = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def do_GET(self):
        u = urlparse(self.path); q = parse_qs(u.query)
        proto = q.get("protocol", [""])[0]; sid = q.get("server_id", [""])[0]
        if u.path == "/v1/server/config":
            cfg = NODE_TABLE.get((proto, sid))
            if cfg is None:
                return self._send_json({"error": f"no node for {proto}/{sid}"}, 404)
            return self._send_json(cfg)
        if u.path == "/v1/server/user":      return self._send_json(USERS)
        if u.path == "/v1/server/alivelist": return self._send_json(ALIVE)
        return self._send_json({"error": "not found"}, 404)
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        if n: self.rfile.read(n)
        return self._send_json({"ok": True})
    def log_message(self, fmt, *args):
        sys.stderr.write("[fake_panel] %s\n" % (fmt % args))

if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 9999), Handler).serve_forever()
PYEOF
    chmod +x "$FAKE_PANEL_FILE"
}

start_fake_panel() {
    if [[ -f "$FAKE_PANEL_PID_FILE" ]] && kill -0 "$(<"$FAKE_PANEL_PID_FILE")" 2>/dev/null; then
        info "fake panel 已在运行 (PID $(<"$FAKE_PANEL_PID_FILE"))"
        return 0
    fi
    write_fake_panel_py
    nohup python3 "$FAKE_PANEL_FILE" >"$FAKE_PANEL_LOG_FILE" 2>&1 &
    echo $! > "$FAKE_PANEL_PID_FILE"
    sleep 1
    if curl -s --max-time 2 "http://127.0.0.1:${FAKE_PANEL_PORT}/v1/server/user" >/dev/null; then
        ok "fake panel 启动成功 (PID $(<"$FAKE_PANEL_PID_FILE"))"
    else
        fail "fake panel 启动失败 — 查看 $FAKE_PANEL_LOG_FILE"
        return 1
    fi
}

write_fake_test_config() {
    cat > "$FAKE_TEST_CONFIG" <<'JSON'
{
    "Log": {"Level": "debug"},
    "Cores": [{"Type": "xray"}, {"Type": "sing"}],
    "Nodes": [
        {
            "ApiHost": "http://127.0.0.1:9999",
            "ApiKey":  "dummy",
            "NodeID":  101,
            "NodeType": "vless",
            "Timeout": 5,
            "ListenIP": "0.0.0.0",
            "CertConfig": {
                "CertMode":   "self",
                "CertDomain": "vless.test",
                "CertFile":   "/etc/yunzes-node/certs/vless101.crt",
                "KeyFile":    "/etc/yunzes-node/certs/vless101.key",
                "RenewBeforeDays": 30
            }
        },
        {
            "ApiHost": "http://127.0.0.1:9999",
            "ApiKey":  "dummy",
            "NodeID":  102,
            "NodeType": "shadowsocks",
            "Timeout": 5,
            "ListenIP": "0.0.0.0"
        },
        {
            "ApiHost": "http://127.0.0.1:9999",
            "ApiKey":  "dummy",
            "NodeID":  103,
            "NodeType": "hysteria2",
            "Timeout": 5,
            "ListenIP": "0.0.0.0",
            "CertConfig": {
                "CertMode":   "self",
                "CertDomain": "hy2.test",
                "CertFile":   "/etc/yunzes-node/certs/hy2103.crt",
                "KeyFile":    "/etc/yunzes-node/certs/hy2103.key",
                "RenewBeforeDays": 30
            }
        },
        {
            "ApiHost": "http://127.0.0.1:9999",
            "ApiKey":  "dummy",
            "NodeID":  104,
            "NodeType": "vless",
            "Timeout": 5,
            "ListenIP": "0.0.0.0"
        }
    ]
}
JSON
}

cmd_fake_test() {
    parse_flags "$@"
    # Default for fake-test is --restart no to avoid the infinite restart loop
    # when a panic happens.
    local effective_restart="no"
    if (( NO_RESTART == 0 )); then
        effective_restart="no"
    fi

    if ! command -v python3 >/dev/null 2>&1; then
        fail "缺少 python3 — apt install -y python3"
        return 1
    fi
    if ! command -v curl >/dev/null 2>&1; then
        fail "缺少 curl"; return 1
    fi
    detect_docker_state >/dev/null
    case $? in
        0) ;;
        10) fail "Docker 未安装"; return 1 ;;
        11) fail "Docker daemon 未运行"; return 1 ;;
        12) fail "无 docker socket 权限"; return 1 ;;
    esac
    if ! docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        fail "镜像 $DEFAULT_IMAGE 不存在；先 yunzes-node install 或 docker build"
        return 1
    fi

    ensure_dirs

    # Save & swap config
    if [[ -f "$CONFIG_FILE" ]]; then
        local pre
        pre=$(backup_now)
        info "fake-test 前已备份原配置：$pre"
    fi
    write_fake_test_config
    cp -f "$FAKE_TEST_CONFIG" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    ok "已写入 4 协议测试配置 → $CONFIG_FILE"

    # Start fake panel
    start_fake_panel || return 1

    # Restart container in test mode
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    step "启动测试容器（restart=$effective_restart）"
    if ! _docker_run "$DEFAULT_IMAGE" "$effective_restart"; then
        fail "测试容器启动失败"
        return 1
    fi

    # Wait & verify
    step "等待 5 秒..."; sleep 5
    echo
    local logs
    logs="$(docker logs "$NAME" 2>&1 || true)"
    local fake_pass=0 fake_fail=0
    local _bad
    _bad=$(echo "$logs" | grep -E -i 'panic|nil pointer|runtime error|segmentation violation' | head -3 || true)
    if [[ -z "$_bad" ]]; then
        ok "无 panic / nil pointer / runtime error"
        fake_pass=$((fake_pass+1))
    else
        fail "发现致命错误："
        echo "$_bad" | sed 's/^/      /'
        fake_fail=$((fake_fail+1))
    fi
    local marker
    for marker in "Core Selector" "Adding node inbound" "logical_tag" "core=" "runtime_key" "protocol=" "server_id" "port="; do
        if echo "$logs" | grep -qF "$marker"; then
            ok "日志含字段：$marker"
            fake_pass=$((fake_pass+1))
        else
            fail "日志未见：$marker"
            fake_fail=$((fake_fail+1))
        fi
    done

    # Port-listen verification
    if command -v ss >/dev/null 2>&1; then
        echo
        step "端口监听检查（host network）"
        local check
        for check in "8101 tcp" "8102 tcp" "8102 udp" "8103 udp" "8104 tcp"; do
            local port proto
            read -r port proto <<<"$check"
            local hits
            case "$proto" in
                tcp) hits=$(ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
                udp) hits=$(ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
            esac
            if [[ -n "$hits" ]] && echo "$hits" | grep -q yunzes-node; then
                ok "$port/$proto 由 yunzes-node 监听"
                fake_pass=$((fake_pass+1))
            else
                fail "$port/$proto 未由 yunzes-node 监听"
                fake_fail=$((fake_fail+1))
            fi
        done
    else
        warn "缺少 ss，跳过端口监听检查"
    fi

    # Cert-action check
    echo
    step "证书复用检查（restart 后应 reuse）"
    docker restart "$NAME" >/dev/null 2>&1 || true
    sleep 3
    local cert_lines
    cert_lines=$(docker logs --tail 200 "$NAME" 2>&1 | grep cert_action || true)
    if echo "$cert_lines" | grep -q 'cert_action=reuse'; then
        ok "重启后 cert_action=reuse（C3 持久化生效）"
        fake_pass=$((fake_pass+1))
    else
        warn "未在重启日志里找到 cert_action=reuse"
        echo "$cert_lines" | sed 's/^/      /'
    fi

    echo
    banner_line
    info "fake-test 汇总：${C_GREEN}PASS=$fake_pass${C_PLAIN}  ${C_RED}FAIL=$fake_fail${C_PLAIN}"
    banner_line

    # Cleanup interaction
    echo
    if confirm "停止 fake panel" y; then
        cmd_stop_fake_panel
    fi
    if confirm "删除测试容器（$NAME）" n; then
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        ok "测试容器已删除"
    fi
    if confirm "保留测试证书（/etc/yunzes-node/certs/*）" y; then
        info "保留 — 后续真实部署会被 EnsureCertificate 按域名匹配复用或拒绝"
    else
        rm -f "$CERTS_DIR"/vless101.* "$CERTS_DIR"/hy2103.*
        ok "测试证书已删除"
    fi
    echo
    if confirm "恢复 fake-test 之前的 config.json" y; then
        local last
        last=$(list_backups | head -1)
        if [[ -n "$last" && -f "$BACKUP_DIR/$last/config.json" ]]; then
            cp -p "$BACKUP_DIR/$last/config.json" "$CONFIG_FILE"
            ok "已恢复：$BACKUP_DIR/$last/config.json"
        else
            warn "未找到 fake-test 前的备份；保留测试 config.json"
        fi
    fi
    return $fake_fail
}

cmd_stop_fake_panel() {
    if [[ -f "$FAKE_PANEL_PID_FILE" ]]; then
        local pid
        pid=$(<"$FAKE_PANEL_PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null && ok "fake panel 已停止 (PID $pid)"
        fi
        rm -f "$FAKE_PANEL_PID_FILE"
    fi
    # belt-and-suspenders: kill anything else listening on 9999 from python
    pkill -f fake_panel.py 2>/dev/null || true
}

# -----------------------------------------------------------------------------
# Uninstall
# -----------------------------------------------------------------------------
cmd_uninstall() {
    if ! confirm "卸载程序但保留 /etc/yunzes-node 与备份" y; then
        info "已取消"
        return 0
    fi
    if container_exists; then
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        ok "容器已删除"
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        if confirm "同时删除镜像 $DEFAULT_IMAGE" n; then
            docker rmi -f "$DEFAULT_IMAGE" >/dev/null 2>&1 && ok "镜像已删除"
        fi
    fi
    if [[ -f "$INSTALLED_PATH" ]] && confirm "删除全局命令 $INSTALLED_PATH" n; then
        rm -f "$INSTALLED_PATH"
        ok "$INSTALLED_PATH 已删除"
    fi
    info "保留：$CONFIG_DIR  $CERTS_DIR  $BACKUP_DIR"
    info "如需彻底清理：yunzes-node uninstall-full"
}

cmd_uninstall_full() {
    fail "此操作将删除以下所有内容："
    fail "  - 容器 $NAME"
    fail "  - 镜像 $DEFAULT_IMAGE"
    fail "  - 配置 $CONFIG_DIR (含证书)"
    fail "  - 运行目录 $RUN_DIR (含全部备份)"
    fail "  - 命令入口 $INSTALLED_PATH"
    echo
    if ! confirm_phrase "请输入 ${C_RED}DELETE YUNZES NODE${C_PLAIN} 进行确认" "DELETE YUNZES NODE"; then
        info "已取消"
        return 0
    fi
    cmd_stop_fake_panel
    if container_exists; then
        docker rm -f "$NAME" >/dev/null 2>&1 || true
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        docker rmi -f "$DEFAULT_IMAGE" >/dev/null 2>&1 || true
    fi
    rm -rf "$CONFIG_DIR" "$RUN_DIR"
    rm -f "$INSTALLED_PATH"
    ok "彻底卸载完成"
}

cmd_setup_entry() {
    if ! is_root; then
        fail "需 root 才能写 $INSTALLED_PATH"
        return 1
    fi
    if [[ ! -f "$SCRIPT_PATH" ]]; then
        fail "找不到当前脚本路径：$SCRIPT_PATH"
        return 1
    fi
    install -m 0755 "$SCRIPT_PATH" "$INSTALLED_PATH"
    ok "命令已安装 / 更新：$INSTALLED_PATH"
    info "现在直接 yunzes-node 即可进入菜单"
}

# -----------------------------------------------------------------------------
# Top-level argument dispatcher
# -----------------------------------------------------------------------------
usage() {
    cat <<EOF
yunzes-node v${SCRIPT_VERSION}  —  单容器双核心 Docker 部署

用法：
    yunzes-node                       # 进入交互菜单
    yunzes-node menu                  # 同上
    yunzes-node install [--no-restart]
    yunzes-node update                # 同 upgrade
    yunzes-node upgrade
    yunzes-node start | stop | restart
    yunzes-node redeploy [--no-restart]
    yunzes-node status
    yunzes-node logs                  # 最近 100 行
    yunzes-node follow-log
    yunzes-node verify                # 三级验证
    yunzes-node edit-config
    yunzes-node show-config           # 自动隐藏 ApiKey
    yunzes-node gen-config
    yunzes-node check-panel
    yunzes-node ports
    yunzes-node containers
    yunzes-node backup
    yunzes-node rollback
    yunzes-node cleanup-images
    yunzes-node fake-test [--no-restart]
    yunzes-node stop-fake-panel
    yunzes-node uninstall
    yunzes-node uninstall-full
    yunzes-node setup-entry           # 安装到 ${INSTALLED_PATH}

路径约定：
    配置  ${CONFIG_FILE}
    证书  ${CERTS_DIR}
    备份  ${BACKUP_DIR}
    日志  docker logs ${NAME}

详见 README.md。
EOF
}

main() {
    SOURCE_DIR="$(detect_source_dir 2>/dev/null || true)"

    local cmd="${1:-menu}"
    [[ $# -gt 0 ]] && shift

    # Most subcommands need root; the read-only ones do not strictly require it
    # but Docker calls usually do. We only hard-fail on root for destructive
    # paths to keep status / logs / verify usable for non-privileged users
    # who happen to be in the docker group.
    case "$cmd" in
        install|update|upgrade|redeploy|edit-config|gen-config|backup|rollback|fake-test|uninstall|uninstall-full|setup-entry)
            if ! is_root; then
                fail "请使用 root 运行：sudo $SCRIPT_PATH $cmd $*"
                exit 1
            fi
            ;;
    esac

    case "$cmd" in
        menu)            cmd_menu ;;
        install)         cmd_install "$@" ;;
        update|upgrade)  cmd_update "$@" ;;
        start)           cmd_start ;;
        stop)            cmd_stop ;;
        restart)         cmd_restart ;;
        redeploy)        cmd_redeploy "$@" ;;
        status)          cmd_status ;;
        logs)            cmd_logs "${1:-100}" ;;
        follow-log)      cmd_follow_log ;;
        verify)          cmd_verify ;;
        edit-config)     cmd_edit_config ;;
        show-config)     cmd_show_config ;;
        gen-config)      cmd_gen_config ;;
        check-panel)     cmd_check_panel ;;
        ports)           cmd_ports ;;
        containers)      cmd_containers ;;
        backup)          cmd_backup ;;
        rollback)        cmd_rollback ;;
        cleanup-images)  cmd_cleanup_images ;;
        fake-test)       cmd_fake_test "$@" ;;
        stop-fake-panel) cmd_stop_fake_panel ;;
        uninstall)       cmd_uninstall ;;
        uninstall-full)  cmd_uninstall_full ;;
        setup-entry)     cmd_setup_entry ;;
        precheck)        precheck "$@" ;;
        version|-v|--version) echo "yunzes-node script v${SCRIPT_VERSION}" ;;
        help|-h|--help)  usage ;;
        *)               usage; exit 1 ;;
    esac
}

main "$@"
