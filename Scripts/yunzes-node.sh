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
# Color output: every operator-facing message goes through one of the
# print_* functions defined in the OUTPUT HELPERS section. Colors auto-
# disable when stdout is not a TTY or NO_COLOR=1 is set.
#
# This script is self-contained: depends on coreutils, bash 4+, docker,
# jq, curl, tar, and python3 (only for fake-test). Source-tree-only
# features (e.g. `docker build` from local Dockerfile) auto-detect.

set -uo pipefail
IFS=$'\n\t'

readonly SCRIPT_VERSION="1.1.0"
readonly NAME="yunzes-node"
readonly DEFAULT_IMAGE="yunzes-node:latest"
readonly REPO_URL="https://github.com/husibo16/yunzes-node.git"
readonly CONFIG_DIR="/etc/yunzes-node"
readonly CONFIG_FILE="${CONFIG_DIR}/config.json"
readonly CERTS_DIR="${CONFIG_DIR}/certs"
readonly FAKE_CERTS_DIR="${CONFIG_DIR}/fake-test-certs"
readonly RUN_DIR="/opt/yunzes-node"
readonly SRC_DIR="${RUN_DIR}/src"
readonly BACKUP_DIR="${RUN_DIR}/backups"
readonly LOG_DIR="${RUN_DIR}/logs"
readonly STATE_DIR="${RUN_DIR}/state"
readonly INSTALLED_PATH="/usr/bin/${NAME}"
readonly FAKE_PANEL_FILE="/tmp/fake_panel.py"
readonly FAKE_PANEL_PID_FILE="/tmp/fake_panel.pid"
readonly FAKE_PANEL_LOG_FILE="/tmp/fake_panel.log"
readonly FAKE_PANEL_PORT=9999
readonly FAKE_TEST_CONFIG="/tmp/yunzes-fake-config.json"

# -----------------------------------------------------------------------------
# Color initialization. NO_COLOR=1 or non-TTY stdout disables colors so logs
# captured to a file don't gather escape garbage.
# -----------------------------------------------------------------------------
if [[ -t 1 && "${NO_COLOR:-0}" != "1" ]]; then
    readonly COLOR_ENABLED=1
    readonly C_RED=$'\033[0;31m'
    readonly C_GREEN=$'\033[0;32m'
    readonly C_YELLOW=$'\033[0;33m'
    readonly C_BLUE=$'\033[0;34m'
    readonly C_MAGENTA=$'\033[0;35m'
    readonly C_CYAN=$'\033[0;36m'
    readonly C_GRAY=$'\033[0;90m'
    readonly C_BOLD=$'\033[1m'
    readonly C_BOLD_CYAN=$'\033[1;36m'
    readonly C_BOLD_RED=$'\033[1;31m'
    readonly C_BOLD_YELLOW=$'\033[1;33m'
    readonly C_DIM=$'\033[2m'
    readonly C_PLAIN=$'\033[0m'
else
    readonly COLOR_ENABLED=0
    readonly C_RED=""        C_GREEN=""       C_YELLOW=""      C_BLUE=""
    readonly C_MAGENTA=""    C_CYAN=""        C_GRAY=""
    readonly C_BOLD=""       C_BOLD_CYAN=""   C_BOLD_RED=""    C_BOLD_YELLOW=""
    readonly C_DIM=""        C_PLAIN=""
fi

# Per-precheck status counters; precheck functions tally into these.
PRECHECK_PASS=0
PRECHECK_WARN=0
PRECHECK_FAIL=0

# Subcommand-level flags. Reset by parse_flags().
NO_RESTART=0
RESTART_POLICY="always"

# Detected at boot.
SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null || echo "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(dirname "$SCRIPT_PATH")"
SOURCE_DIR=""

# -----------------------------------------------------------------------------
# OUTPUT HELPERS
#
# Every operator-facing line that is not raw command output (docker logs,
# JSON dump, ...) MUST go through one of these. Direct echo / printf is
# reserved for:
#   - blank lines
#   - structured data being piped to / from another tool
#   - inline read prompts (which use printf with explicit color codes)
# -----------------------------------------------------------------------------
print_step()    { printf "%b[STEP]%b %s\n" "${C_CYAN}"    "${C_PLAIN}" "$*"; }
print_info()    { printf "%b[INFO]%b %s\n" "${C_BLUE}"    "${C_PLAIN}" "$*"; }
print_success() { printf "%b[ OK ]%b %s\n" "${C_GREEN}"   "${C_PLAIN}" "$*"; }
print_ok()      { print_success "$@"; }
print_warn()    { printf "%b[WARN]%b %s\n" "${C_YELLOW}"  "${C_PLAIN}" "$*" >&2; }
print_error()   { printf "%b[FAIL]%b %s\n" "${C_RED}"     "${C_PLAIN}" "$*" >&2; }
print_fail()    { print_error "$@"; }
print_fix()     { printf "%b[FIX ]%b %s\n" "${C_YELLOW}"  "${C_PLAIN}" "$*" >&2; }
print_cmd()     { printf "%b[CMD ]%b %s\n" "${C_MAGENTA}" "${C_PLAIN}" "$*"; }
print_danger()  { printf "%b[!!!!]%b %s\n" "${C_BOLD_RED}" "${C_PLAIN}" "$*" >&2; }
print_title()   { printf "%b%s%b\n" "${C_BOLD_CYAN}" "$*" "${C_PLAIN}"; }
print_kv()      {
    # print_kv "label" "value" — label cyan, value blue
    printf "  %b%s%b: %b%s%b\n" "${C_CYAN}" "$1" "${C_PLAIN}" "${C_BLUE}" "$2" "${C_PLAIN}"
}
print_separator() {
    printf "%b%s%b\n" "${C_DIM}${C_CYAN}" "──────────────────────────────────────────────" "${C_PLAIN}"
}
print_menu_item() {
    # print_menu_item NUM "label" [danger|exit]
    local num="$1" label="$2" flag="${3:-}"
    case "$flag" in
        danger) printf "  %b%2s%b) %b%s%b\n" "${C_GREEN}" "$num" "${C_PLAIN}" "${C_BOLD_YELLOW}" "$label" "${C_PLAIN}" ;;
        exit)   printf "  %b%2s%b) %b%s%b\n" "${C_GREEN}" "$num" "${C_PLAIN}" "${C_GRAY}"        "$label" "${C_PLAIN}" ;;
        *)      printf "  %b%2s%b) %b%s%b\n" "${C_GREEN}" "$num" "${C_PLAIN}" "${C_CYAN}"        "$label" "${C_PLAIN}" ;;
    esac
}
print_choice() {
    # print_choice "NN)" "description" — coloured choice prompt
    printf "  %b%s%b %b%s%b\n" "${C_GREEN}" "$1" "${C_PLAIN}" "${C_CYAN}" "$2" "${C_PLAIN}"
}

# Backwards-compatible shorter aliases used throughout the rest of the script.
info()    { print_info    "$@"; }
ok()      { print_success "$@"; }
warn()    { print_warn    "$@"; }
fail()    { print_error   "$@"; }
step()    { print_step    "$@"; }
fix_hint(){ print_fix     "$@"; }

# -----------------------------------------------------------------------------
# Interactive helpers
# -----------------------------------------------------------------------------
err_exit() { fail "$*"; exit 1; }

prompt_read() {
    # prompt_read "question" "default" → echos answer to stdout
    local q="$1" def="${2:-}" reply
    if [[ -n "$def" ]]; then
        printf "%b? %s [%s]: %b" "${C_CYAN}" "$q" "$def" "${C_PLAIN}" >&2
    else
        printf "%b? %s: %b"      "${C_CYAN}" "$q"        "${C_PLAIN}" >&2
    fi
    read -r reply || true
    echo "${reply:-$def}"
}

confirm() {
    # confirm "prompt" "default(y|n)"
    local prompt="$1" default="${2:-n}" reply tail
    if [[ "$default" == "y" ]]; then tail="[Y/n]"; else tail="[y/N]"; fi
    printf "%b? %s %s: %b" "${C_CYAN}" "$prompt" "$tail" "${C_PLAIN}" >&2
    read -r reply || return 1
    reply="${reply:-$default}"
    [[ "$reply" =~ ^[Yy]$ ]]
}

confirm_phrase() {
    # confirm_phrase "prompt" "EXACT EXPECTED PHRASE"
    local prompt="$1" expected="$2" reply
    printf "%b? %s: %b" "${C_BOLD_RED}" "$prompt" "${C_PLAIN}" >&2
    read -r reply || return 1
    [[ "$reply" == "$expected" ]]
}

mask_api_key() {
    local k="$1" n=${#1}
    if (( n <= 8 )); then printf '****'; else printf '%s********%s' "${k:0:4}" "${k: -4}"; fi
}

# -----------------------------------------------------------------------------
# Detection helpers
# -----------------------------------------------------------------------------
is_root() { [[ $EUID -eq 0 ]]; }

# detect_os echoes one of: debian, ubuntu, other
#
# Does NOT `source /etc/os-release` because some distributions mark fields
# (NAME, VERSION) as readonly and sourcing into a sub-shell that already
# has a readonly NAME aborts with "readonly variable: NAME". grep|cut is
# safe regardless.
detect_os() {
    local id id_like
    id=$(grep -E '^ID=' /etc/os-release 2>/dev/null \
            | head -1 | cut -d= -f2- | tr -d '"' || true)
    id_like=$(grep -E '^ID_LIKE=' /etc/os-release 2>/dev/null \
            | head -1 | cut -d= -f2- | tr -d '"' || true)
    case "$id" in
        debian) echo debian; return ;;
        ubuntu) echo ubuntu; return ;;
    esac
    case "$id_like" in
        *debian*) echo debian; return ;;
        *ubuntu*) echo ubuntu; return ;;
    esac
    echo other
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *) echo "$(uname -m)" ;;
    esac
}

detect_source_dir() {
    # Echos the source dir if running from a checkout; empty otherwise.
    local d="$SCRIPT_DIR"
    for _ in 1 2; do
        if [[ -f "$d/Dockerfile" && -f "$d/go.mod" ]]; then
            echo "$d"; return 0
        fi
        d="$(dirname "$d")"
    done
    return 1
}

# detect_docker_state returns:
#   0  — docker reachable
#   10 — not installed
#   11 — daemon down
#   12 — current user has no socket permission
detect_docker_state() {
    if ! command -v docker >/dev/null 2>&1; then return 10; fi
    local out rc
    out="$(docker ps 2>&1)" && rc=0 || rc=$?
    [[ $rc -eq 0 ]] && return 0
    if echo "$out" | grep -qiE 'permission denied|/var/run/docker.sock'; then return 12; fi
    if echo "$out" | grep -qiE 'cannot connect to the docker daemon';    then return 11; fi
    return 12
}

container_exists()  { docker ps -a --format '{{.Names}}' 2>/dev/null | grep -Fxq "$NAME"; }
container_running() { docker ps    --format '{{.Names}}' 2>/dev/null | grep -Fxq "$NAME"; }
current_image_id()  { docker inspect --format '{{.Image}}'        "$NAME" 2>/dev/null || true; }
current_image_name(){ docker inspect --format '{{.Config.Image}}' "$NAME" 2>/dev/null || true; }

# -----------------------------------------------------------------------------
# Flag parsing — strips --no-restart from $@.
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
    printf "%b" "${C_BOLD_CYAN}"
    cat <<'BANNER'

   __ __ _   _ _ _____  ___ ___    __ _  ___  ___  ___
   \ V / | | | | |_  / / __/ __|__/ /| || _ \| _ \| __|
    | || |_| |_| |/ /  \__ \__ \__/ /_| || _ /| _ /| _|
    |_||___\___/|_/___||___/___/_/  __\__|___||___||___|

BANNER
    printf "%b" "${C_PLAIN}"
    print_kv "version"    "v${SCRIPT_VERSION}"
    print_kv "image"      "${DEFAULT_IMAGE}"
    print_kv "config dir" "${CONFIG_DIR}"
    print_kv "run dir"    "${RUN_DIR}"
    if [[ "$COLOR_ENABLED" == "0" ]]; then
        print_info "颜色已禁用 (NO_COLOR=1 或非交互式 stdout)"
    fi
}

print_menu() {
    echo
    print_title  "管理菜单"
    print_separator
    print_menu_item  1 "安装 yunzes-node"
    print_menu_item  2 "升级 yunzes-node"
    print_menu_item  3 "启动 yunzes-node"
    print_menu_item  4 "停止 yunzes-node"
    print_menu_item  5 "重启 yunzes-node"
    print_menu_item  6 "重新部署容器"
    print_menu_item  7 "查看运行状态"
    print_menu_item  8 "查看实时日志"
    print_menu_item  9 "查看最近日志"
    print_menu_item 10 "验证节点服务"
    print_menu_item 11 "编辑配置文件"
    print_menu_item 12 "查看当前配置 (隐藏 ApiKey)"
    print_menu_item 13 "生成配置模板"
    print_menu_item 14 "测试连接 panel server"
    print_menu_item 15 "查看监听端口"
    print_menu_item 16 "查看 Docker 容器信息"
    print_menu_item 17 "备份当前配置"
    print_menu_item 18 "回滚到上一个备份"
    print_menu_item 19 "清理旧镜像"                   danger
    print_menu_item 20 "运行 fake panel 四协议验证"
    print_menu_item 21 "停止 fake panel"
    print_menu_item 22 "卸载程序，保留配置和证书"     danger
    print_menu_item 23 "完全卸载（删除配置 + 证书）"   danger
    print_menu_item 24 "安装/更新命令入口 ${INSTALLED_PATH}"
    print_menu_item 25 "退出"                         exit
    print_separator
}

cmd_menu() {
    while true; do
        clear || true
        print_banner
        print_menu
        local choice
        printf "%b请选择 [1-25]: %b" "${C_CYAN}" "${C_PLAIN}"
        read -r choice || break
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
            25|q|Q|exit) print_info "再见。"; return 0 ;;
            *)  print_warn "无效选项：$choice" ;;
        esac
        echo
        printf "%b按回车返回菜单...%b" "${C_DIM}" "${C_PLAIN}"
        read -r _ || true
    done
}

# -----------------------------------------------------------------------------
# PreCheck split into basic / dependency / docker / file phases. cmd_install
# calls them in order; `yunzes-node precheck` runs the whole pipeline.
# -----------------------------------------------------------------------------
_pcok()   { print_success "$1"; PRECHECK_PASS=$((PRECHECK_PASS+1)); }
_pcwarn() { print_warn    "$1"; PRECHECK_WARN=$((PRECHECK_WARN+1)); }
_pcfail() { print_error   "$1"; PRECHECK_FAIL=$((PRECHECK_FAIL+1)); }

reset_precheck_counters() {
    PRECHECK_PASS=0; PRECHECK_WARN=0; PRECHECK_FAIL=0
}

basic_precheck() {
    print_step "PreCheck: 基础环境"
    if is_root; then
        _pcok "运行用户：root"
    else
        _pcfail "必须使用 root 运行（当前 UID=$EUID）"
        print_fix "改用：sudo $SCRIPT_PATH ${1:-menu}"
    fi
    local os arch
    os=$(detect_os)
    case "$os" in
        debian|ubuntu) _pcok "操作系统：$os" ;;
        *)             _pcwarn "未在 Debian / Ubuntu 上测试（detected: $os），脚本可能仍能运行" ;;
    esac
    arch=$(detect_arch)
    case "$arch" in
        amd64|arm64) _pcok "CPU 架构：$arch" ;;
        *)           _pcwarn "未测试的 CPU 架构：$arch（amd64 / arm64 之外的平台风险自担）" ;;
    esac
    if command -v free >/dev/null 2>&1; then
        local mem_free
        mem_free=$(free -h --si 2>/dev/null | awk '/^Mem:/{print $7}')
        [[ -n "$mem_free" ]] && _pcok "可用内存：$mem_free"
    fi
    local disk_free
    disk_free=$(df -h / 2>/dev/null | awk 'NR==2{print $4}')
    [[ -n "$disk_free" ]] && _pcok "/ 可用磁盘：$disk_free"
}

DEPENDENCIES=(curl jq tar git ss python3)
DEP_PACKAGES=(curl jq tar git iproute2 python3)

dependency_precheck() {
    print_step "PreCheck: CLI 依赖"
    local tool missing=0
    for tool in "${DEPENDENCIES[@]}"; do
        if command -v "$tool" >/dev/null 2>&1; then
            _pcok "命令存在：$tool"
        else
            _pcwarn "缺少 $tool（ensure_dependencies 阶段会询问安装）"
            missing=$((missing+1))
        fi
    done
    return 0
}

ensure_dependencies() {
    # Returns 0 if all deps present (or successfully installed), 1 if user
    # declined or install failed. Also installs docker.io when missing and
    # starts the daemon. Debian / Ubuntu only; on other distros we just
    # warn and let docker_precheck do the hard fail.
    local os
    os=$(detect_os)
    if [[ "$os" != "debian" && "$os" != "ubuntu" ]]; then
        print_warn "非 Debian/Ubuntu 系统，跳过自动安装；请手动确保依赖已就绪"
        return 0
    fi

    print_step "ensure_dependencies: 检查并安装必需依赖"
    local missing=()
    local i
    for i in "${!DEPENDENCIES[@]}"; do
        local tool="${DEPENDENCIES[$i]}"
        if ! command -v "$tool" >/dev/null 2>&1; then
            missing+=("${DEP_PACKAGES[$i]}")
        fi
    done
    if ! command -v docker >/dev/null 2>&1; then
        missing+=("docker.io")
    fi

    if (( ${#missing[@]} == 0 )); then
        print_ok "所有依赖已就绪"
    else
        print_info "缺失依赖: ${missing[*]}"
        if confirm "执行 apt update && apt install -y ${missing[*]}" y; then
            print_cmd "apt-get update -y"
            DEBIAN_FRONTEND=noninteractive apt-get update -y || {
                print_fail "apt update 失败 — 请检查网络或 /etc/apt/sources.list"
                return 1
            }
            print_cmd "apt-get install -y ${missing[*]}"
            DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" || {
                print_fail "apt install 失败 — 请手动 apt install -y ${missing[*]} 后重试"
                return 1
            }
            print_ok "依赖安装完成"
        else
            print_warn "用户取消自动安装；请手动安装后重试"
            print_fix "apt update && apt install -y ${missing[*]}"
            return 1
        fi
    fi

    # Make sure dockerd is running.
    if command -v docker >/dev/null 2>&1; then
        if ! docker info >/dev/null 2>&1; then
            print_step "尝试启动 Docker daemon"
            if command -v systemctl >/dev/null 2>&1; then
                print_cmd "systemctl start docker"
                systemctl start docker 2>/dev/null || true
                systemctl enable docker 2>/dev/null || true
            else
                print_cmd "service docker start"
                service docker start 2>/dev/null || true
            fi
            sleep 2
            if docker info >/dev/null 2>&1; then
                print_ok "Docker daemon 已启动"
            else
                print_warn "Docker daemon 启动失败（可能需要手动 service docker start）"
            fi
        fi
    fi
    return 0
}

docker_precheck() {
    print_step "PreCheck: Docker"
    detect_docker_state
    case $? in
        0)
            _pcok "Docker 可用且当前用户能调用 docker ps"
            ;;
        10)
            _pcfail "Docker 未安装"
            print_fix "Debian/Ubuntu 安装：apt update && apt install -y docker.io"
            ;;
        11)
            _pcfail "Docker daemon 未运行"
            print_fix "启动 daemon：service docker start  或  systemctl start docker"
            ;;
        12)
            _pcfail "当前用户无 /var/run/docker.sock 权限"
            print_fix "永久路：usermod -aG docker \$USER  然后重新登录终端"
            print_fix "临时路：用 sudo 重跑此脚本，或切到 root"
            ;;
    esac
    if docker compose version >/dev/null 2>&1; then
        _pcok "docker compose 可用 (v2 plugin)"
    elif command -v docker-compose >/dev/null 2>&1; then
        _pcok "docker-compose 可用 (v1 binary)"
    else
        _pcwarn "未发现 docker compose（不影响一键脚本，docker run 直跑即可）"
    fi
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
}

file_precheck() {
    print_step "PreCheck: 文件与目录"
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
            print_fix "用菜单 11 / yunzes-node edit-config 修复"
        fi
    else
        _pcwarn "config.json 不存在：$CONFIG_FILE（安装流程会引导生成）"
    fi
    _check_port_free 80  tcp || true
    _check_port_free 443 tcp || true
    if [[ -f "$CONFIG_FILE" ]] && jq empty "$CONFIG_FILE" 2>/dev/null; then
        local tmp spec addr port proto
        tmp=$(mktemp)
        list_config_listen_specs > "$tmp" || true
        while IFS= read -r spec; do
            [[ -z "$spec" ]] && continue
            IFS=':/' read -r addr port proto <<<"$spec"
            # Local config doesn't carry per-protocol port (panel-driven);
            # placeholder "?" must NEVER reach _check_port_free.
            [[ "$port" == "?" ]] && continue
            [[ -z "$port" || -z "$proto" ]] && continue
            _check_port_free "$port" "$proto" || true
        done < "$tmp"
        rm -f "$tmp"
    fi
    if command -v ufw >/dev/null 2>&1; then
        local ufw_state
        ufw_state="$(ufw status 2>/dev/null | head -1 || true)"
        _pcwarn "检测到 ufw（$ufw_state）；请确保业务端口已放行，本脚本不主动改动防火墙"
    elif command -v firewall-cmd >/dev/null 2>&1; then
        _pcwarn "检测到 firewalld；请确保业务端口已放行，本脚本不主动改动防火墙"
    fi
}

precheck() {
    reset_precheck_counters
    basic_precheck      "${1:-}"
    dependency_precheck
    docker_precheck
    file_precheck
    echo
    print_info "PreCheck 汇总:"
    printf "  %bPASS%b: %b%d%b\n" "${C_GREEN}"  "${C_PLAIN}" "${C_GREEN}"  "$PRECHECK_PASS" "${C_PLAIN}"
    printf "  %bWARN%b: %b%d%b\n" "${C_YELLOW}" "${C_PLAIN}" "${C_YELLOW}" "$PRECHECK_WARN" "${C_PLAIN}"
    printf "  %bFAIL%b: %b%d%b\n" "${C_RED}"    "${C_PLAIN}" "${C_RED}"    "$PRECHECK_FAIL" "${C_PLAIN}"
    if (( PRECHECK_FAIL > 0 )); then
        print_fail "PreCheck 不通过，安装/升级流程中断。修完上面的 [FAIL] 项再重试。"
        return 1
    fi
    return 0
}

_check_port_free() {
    # _check_port_free PORT TRANSPORT
    local port="$1" proto="$2"
    [[ "$port" == "?" || -z "$port" ]] && return 0
    if ! command -v ss >/dev/null 2>&1; then return 0; fi
    local hits owner
    case "$proto" in
        tcp) hits=$(ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
        udp) hits=$(ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
        *)   return 0 ;;
    esac
    if [[ -z "$hits" ]]; then return 0; fi
    owner=$(echo "$hits" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
    if [[ "$owner" == "$NAME" ]]; then
        _pcok "$port/$proto 已由 yunzes-node 自身监听（属正常）"
    else
        _pcwarn "$port/$proto 已被 ${owner:-未知进程} 占用；ACME / 节点端口可能冲突"
    fi
}

# -----------------------------------------------------------------------------
# Config helpers — list listen specs, etc.
# Output of list_config_listen_specs: one "addr:?/proto" per line.
# Port placeholder "?" is intentional because local config doesn't carry the
# panel-driven port; real port-listen verification reads docker logs instead.
# -----------------------------------------------------------------------------
list_config_listen_specs() {
    [[ -f "$CONFIG_FILE" ]] || return 0
    jq empty "$CONFIG_FILE" 2>/dev/null || return 1
    local idx total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE" 2>/dev/null)
    [[ -z "$total" ]] && total=0
    for ((idx=0; idx<total; idx++)); do
        local node_type listen_ip
        node_type=$(jq -r ".Nodes[$idx].NodeType // empty" "$CONFIG_FILE")
        listen_ip=$(jq -r ".Nodes[$idx].Options.ListenIP // .Nodes[$idx].ListenIP // \"0.0.0.0\"" "$CONFIG_FILE")
        [[ -z "$listen_ip" || "$listen_ip" == "null" ]] && listen_ip="0.0.0.0"
        case "$node_type" in
            shadowsocks)             printf '%s:?/tcp\n%s:?/udp\n' "$listen_ip" "$listen_ip" ;;
            hysteria|hysteria2|tuic) printf '%s:?/udp\n' "$listen_ip" ;;
            vless|vmess|trojan|anytls) printf '%s:?/tcp\n' "$listen_ip" ;;
        esac
    done
}

# -----------------------------------------------------------------------------
# Backup / Restore
# -----------------------------------------------------------------------------
backup_now() {
    mkdir -p "$BACKUP_DIR"
    local ts dir
    ts=$(date +%Y%m%d-%H%M%S)
    dir="$BACKUP_DIR/$ts"
    mkdir -p "$dir"
    if [[ -f "$CONFIG_FILE" ]]; then
        cp -p "$CONFIG_FILE" "$dir/config.json"
    fi
    if [[ -d "$CERTS_DIR" ]]; then
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
    local name="$1" dir="$BACKUP_DIR/$1"
    [[ -d "$dir" ]] || { print_fail "备份目录不存在：$dir"; return 1; }
    print_step "回滚到备份：$name"
    if [[ -f "$dir/config.json" ]]; then
        cp -p "$dir/config.json" "$CONFIG_FILE"
        print_ok "config.json 恢复"
    fi
    if [[ -f "$dir/certs.tar.gz" ]]; then
        tar -C "$CONFIG_DIR" -xzf "$dir/certs.tar.gz" 2>/dev/null \
            || print_warn "证书包解压失败（可手动 tar -xzvf 查看）"
        print_ok "certs/ 恢复"
    fi
    local image_to_use=""
    [[ -f "$dir/image-name.txt" ]] && image_to_use="$(<"$dir/image-name.txt")"
    [[ -z "$image_to_use" ]] && image_to_use="$DEFAULT_IMAGE"
    print_info "使用镜像：$image_to_use"
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if ! _docker_run "$image_to_use" "always"; then
        print_fail "回滚启动容器失败"
        return 1
    fi
    return 0
}

# -----------------------------------------------------------------------------
# Source-tree management — clone / pull from REPO_URL.
# -----------------------------------------------------------------------------
fetch_or_use_source() {
    # Echos a directory path on stdout; status-only output goes to stderr so
    # callers can `local d=$(fetch_or_use_source)`.
    if [[ -d "$SRC_DIR/.git" ]]; then
        print_info "已存在源码：$SRC_DIR" >&2
        print_choice "1)" "git pull (拉取最新)"          >&2
        print_choice "2)" "使用现有源码"                 >&2
        print_choice "3)" "删除后重新 clone"             >&2
        print_choice "4)" "退出"                         >&2
        local ans
        printf "%b请选择 [1-4]: %b" "${C_CYAN}" "${C_PLAIN}" >&2
        read -r ans
        case "$ans" in
            1)
                print_cmd "git -C $SRC_DIR pull --ff-only" >&2
                if ! ( cd "$SRC_DIR" && git pull --ff-only ) >&2; then
                    print_warn "git pull 失败（可能本地有未提交改动）" >&2
                    if ! confirm "继续使用当前 $SRC_DIR 源码" y; then
                        return 1
                    fi
                fi
                ;;
            2) print_info "使用现有 $SRC_DIR" >&2 ;;
            3)
                print_danger "将删除 $SRC_DIR 重新 clone" >&2
                if ! confirm "继续" n; then
                    return 1
                fi
                print_cmd "rm -rf $SRC_DIR" >&2
                rm -rf "$SRC_DIR"
                print_cmd "git clone $REPO_URL $SRC_DIR" >&2
                git clone "$REPO_URL" "$SRC_DIR" >&2 || return 1
                ;;
            4) return 1 ;;
            *) print_warn "无效选项，使用现有源码" >&2 ;;
        esac
    else
        mkdir -p "$(dirname "$SRC_DIR")"
        print_cmd "git clone $REPO_URL $SRC_DIR" >&2
        git clone "$REPO_URL" "$SRC_DIR" >&2 || return 1
    fi
    echo "$SRC_DIR"
}

# -----------------------------------------------------------------------------
# Container ops
# -----------------------------------------------------------------------------
_docker_run() {
    # _docker_run IMAGE RESTART_POLICY [extra-args ...]
    local image="$1" restart="${2:-always}"; shift 2 || true
    print_cmd "docker run -d --name $NAME --network host --restart $restart -v $CONFIG_DIR:/etc/yunzes-node $* $image"
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
# Verify L1 / L2 / L3
# -----------------------------------------------------------------------------
verify_basic() {
    local pass=0 myfail=0
    print_step "Verify L1: 基础"
    if container_running; then
        print_ok "容器 $NAME 运行中"; pass=$((pass+1))
    else
        print_fail "容器 $NAME 未运行"; myfail=$((myfail+1))
    fi
    if [[ -f "$CONFIG_FILE" ]]; then
        if jq empty "$CONFIG_FILE" 2>/dev/null; then
            print_ok "config.json 存在且合法"; pass=$((pass+1))
        else
            print_fail "config.json 存在但非合法 JSON"; myfail=$((myfail+1))
        fi
    else
        print_fail "config.json 不存在"; myfail=$((myfail+1))
    fi
    if [[ -d "$CERTS_DIR" ]]; then
        print_ok "certs/ 存在"; pass=$((pass+1))
    else
        print_warn "certs/ 不存在（仅 cleartext / reality 节点可接受）"
    fi
    local bad
    bad=$(docker logs "$NAME" 2>&1 | grep -E -i 'panic|runtime error|nil pointer dereference|segmentation violation|fatal error' | head -5 || true)
    if [[ -z "$bad" ]]; then
        print_ok "docker logs 无 panic / fatal / runtime error"; pass=$((pass+1))
    else
        print_fail "docker logs 出现 panic / fatal / runtime error:"
        print_info "以下为匹配到的原始日志行:"
        echo "$bad" | sed 's/^/      /'
        myfail=$((myfail+1))
    fi
    printf "L1: %bPASS=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

# extract_listens_from_logs — parse `Adding node inbound` lines and emit
# "PROTOCOL PORT TRANSPORT" rows. Lines are well-formed logrus key=value:
# `... msg="Adding node inbound" core=xray network="[tcp udp]" port=8102 protocol=shadowsocks ...`
extract_listens_from_logs() {
    docker logs --tail 1000 "$NAME" 2>&1 \
        | grep -F 'msg="Adding node inbound"' \
        | while IFS= read -r line; do
            local port proto network
            port=$(echo    "$line" | grep -oE 'port=[0-9]+'                     | head -1 | cut -d= -f2)
            proto=$(echo   "$line" | grep -oE 'protocol=[a-z0-9]+'              | head -1 | cut -d= -f2)
            network=$(echo "$line" | grep -oE 'network="\[[^]]+\]"'             | head -1 | sed -E 's/network="\[([^]]+)\]"/\1/')
            [[ -z "$port" || -z "$network" ]] && continue
            local t
            for t in $network; do
                echo "$proto $port $t"
            done
        done
}

_listen_active() {
    # _listen_active PORT TRANSPORT — returns 0 iff something is listening.
    local port="$1" proto="$2"
    case "$proto" in
        tcp) ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' | grep -q .;;
        udp) ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' | grep -q .;;
        *)   return 1;;
    esac
}

verify_network() {
    local pass=0 myfail=0 mywarn=0
    print_step "Verify L2: 网络"
    local mode
    mode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$NAME" 2>/dev/null || true)
    if [[ "$mode" == "host" ]]; then
        print_ok "容器使用 host network"; pass=$((pass+1))
    elif [[ -z "$mode" ]]; then
        print_warn "无法读取容器网络模式（容器可能不存在）"; mywarn=$((mywarn+1))
    else
        print_warn "容器使用 $mode 而非 host network（生产建议 host）"; mywarn=$((mywarn+1))
    fi
    if [[ ! -f "$CONFIG_FILE" ]] || ! command -v ss >/dev/null 2>&1; then
        printf "L2: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
            "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
        return $myfail
    fi
    # Real port-listen check by parsing docker logs.
    local rows row p port t
    rows="$(extract_listens_from_logs)"
    if [[ -z "$rows" ]]; then
        print_warn "docker logs 未找到 'Adding node inbound' 行（容器尚未上线节点？）"
        mywarn=$((mywarn+1))
    else
        while IFS= read -r row; do
            [[ -z "$row" ]] && continue
            read -r p port t <<<"$row"
            if _listen_active "$port" "$t"; then
                print_ok "$p $port/$t 实际监听 ✓"; pass=$((pass+1))
            else
                print_fail "$p $port/$t 期待监听但未发现"; myfail=$((myfail+1))
            fi
        done <<<"$rows"
        # Shadowsocks dual-listen sanity: count tcp+udp pairs.
        local ss_tcp ss_udp
        ss_tcp=$(echo "$rows" | awk '$1=="shadowsocks" && $3=="tcp"' | wc -l)
        ss_udp=$(echo "$rows" | awk '$1=="shadowsocks" && $3=="udp"' | wc -l)
        if (( ss_tcp + ss_udp > 0 )); then
            if (( ss_tcp == ss_udp )); then
                print_ok "shadowsocks 节点 tcp+udp 配对一致 (各 $ss_tcp 个)"; pass=$((pass+1))
            else
                print_fail "shadowsocks tcp/udp 不配对 (tcp=$ss_tcp udp=$ss_udp)"; myfail=$((myfail+1))
            fi
        fi
        # Hysteria2/Tuic must be UDP-only.
        local udp_only
        udp_only=$(echo "$rows" | awk '$1 ~ /^(hysteria2?|tuic)$/ && $3=="udp"' | wc -l)
        if (( udp_only > 0 )); then
            print_ok "hysteria2 / tuic 节点 udp 监听 (共 $udp_only 个)"; pass=$((pass+1))
        fi
    fi
    # ACME 80/tcp hint
    local hits owner80
    hits=$(ss -lntp 2>/dev/null | awk '$4 ~ /:80$/' || true)
    if [[ -n "$hits" ]]; then
        owner80=$(echo "$hits" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
        if [[ "$owner80" == "$NAME" ]]; then
            print_ok "80/tcp 由 yunzes-node 自身监听"; pass=$((pass+1))
        else
            print_warn "80/tcp 被 ${owner80:-未知} 占用（若用 ACME HTTP-01 会失败）"; mywarn=$((mywarn+1))
        fi
    fi
    printf "L2: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

verify_business() {
    local pass=0 myfail=0 mywarn=0
    print_step "Verify L3: 业务"
    if [[ -f "$CONFIG_FILE" ]] && jq empty "$CONFIG_FILE" 2>/dev/null; then
        local hosts h code
        mapfile -t hosts < <(jq -r '.Nodes[]?.ApiHost // empty' "$CONFIG_FILE" | sort -u)
        for h in "${hosts[@]}"; do
            [[ -z "$h" ]] && continue
            code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$h" || true)
            if [[ "$code" =~ ^[2345] ]]; then
                print_ok "panel 可达：$h  HTTP $code"; pass=$((pass+1))
            else
                print_warn "panel 无响应：$h（curl 返回 $code）"; mywarn=$((mywarn+1))
            fi
        done
    else
        print_warn "无可用 config.json，跳过 panel 探活"; mywarn=$((mywarn+1))
    fi
    local logs
    logs="$(docker logs --tail 500 "$NAME" 2>&1 || true)"
    local marker
    for marker in "Start yunzes-node" "Core Selector" "Adding node inbound" "logical_tag" "core=" "runtime_key" "protocol=" "server_id" "port="; do
        if echo "$logs" | grep -qF "$marker"; then
            print_ok "日志含字段：$marker"; pass=$((pass+1))
        else
            print_warn "日志未见：$marker"; mywarn=$((mywarn+1))
        fi
    done
    printf "L3: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

cmd_verify() {
    verify_basic;    local r1=$?; echo
    verify_network;  local r2=$?; echo
    verify_business; local r3=$?; echo
    print_separator
    if (( r1 + r2 + r3 == 0 )); then
        print_ok "全部 verify 等级通过"; return 0
    fi
    print_fail "verify 有 FAIL 项；查看上方输出"
    return 1
}

# -----------------------------------------------------------------------------
# Install / Update / Lifecycle
# -----------------------------------------------------------------------------
cmd_install() {
    parse_flags "$@"
    reset_precheck_counters
    basic_precheck install
    if (( PRECHECK_FAIL > 0 )); then
        print_fail "basic_precheck 不通过 — 修完上面的 [FAIL] 项再重试"
        return 1
    fi

    if ! ensure_dependencies; then
        return 1
    fi

    reset_precheck_counters
    docker_precheck
    if (( PRECHECK_FAIL > 0 )); then
        print_fail "docker_precheck 不通过 — 检查 Docker daemon / 用户组"
        return 1
    fi

    ensure_dirs

    # 1. config
    if [[ -f "$CONFIG_FILE" ]]; then
        print_info "已检测到 $CONFIG_FILE"
        print_choice "1)" "使用现有配置"
        print_choice "2)" "备份后重新生成"
        print_choice "3)" "退出"
        local ans
        ans=$(prompt_read "请选择" "1")
        case "$ans" in
            1) print_info "使用现有配置" ;;
            2)
                local b
                b=$(backup_now)
                print_ok "已备份到 $b"
                gen_config_template "$CONFIG_FILE"
                if confirm "是否进入交互式生成 (推荐)" y; then
                    gen_config_interactive
                fi
                ;;
            3) print_info "用户取消"; return 0 ;;
            *) print_warn "无效选项，使用现有配置" ;;
        esac
    else
        print_info "config.json 不存在，先生成模板"
        gen_config_template "$CONFIG_FILE.example"
        if confirm "是否进入交互式生成 $CONFIG_FILE" y; then
            gen_config_interactive
        else
            print_warn "请先 cp $CONFIG_FILE.example $CONFIG_FILE 并按需修改，再重跑安装"
            return 0
        fi
    fi

    # 2. file_precheck (advisory)
    file_precheck

    # 3. image source
    if ! docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        print_info "镜像 $DEFAULT_IMAGE 不存在。请选择镜像来源:"
        print_choice "1)" "从 GitHub 拉源码并本地构建 (推荐)"
        print_choice "2)" "使用当前目录源码构建"
        print_choice "3)" "拉取远程 Docker 镜像"
        print_choice "4)" "手动输入镜像名"
        local choice img d
        choice=$(prompt_read "请选择" "1")
        case "$choice" in
            1)
                d=$(fetch_or_use_source) || { print_fail "源码获取失败"; return 1; }
                print_step "在 $d 执行 docker build"
                print_cmd "docker build -t $DEFAULT_IMAGE ."
                if ! ( cd "$d" && docker build -t "$DEFAULT_IMAGE" . ); then
                    print_fail "docker build 失败"
                    return 1
                fi
                print_ok "镜像构建完成: $DEFAULT_IMAGE"
                ;;
            2)
                if [[ -z "$SOURCE_DIR" ]]; then
                    print_fail "未在源码目录运行；请改用选项 1 / 3 / 4"
                    return 1
                fi
                print_cmd "docker build -t $DEFAULT_IMAGE ."
                if ! ( cd "$SOURCE_DIR" && docker build -t "$DEFAULT_IMAGE" . ); then
                    print_fail "docker build 失败"; return 1
                fi
                ;;
            3)
                img=$(prompt_read "拉取的镜像名" "$DEFAULT_IMAGE")
                print_cmd "docker pull $img"
                docker pull "$img" || { print_fail "docker pull 失败"; return 1; }
                if [[ "$img" != "$DEFAULT_IMAGE" ]]; then
                    print_cmd "docker tag $img $DEFAULT_IMAGE"
                    docker tag "$img" "$DEFAULT_IMAGE"
                fi
                print_ok "已 tag 为 $DEFAULT_IMAGE"
                ;;
            4)
                img=$(prompt_read "镜像名" "")
                [[ -z "$img" ]] && { print_fail "镜像名为空"; return 1; }
                print_cmd "docker tag $img $DEFAULT_IMAGE"
                docker tag "$img" "$DEFAULT_IMAGE" || { print_fail "tag 失败"; return 1; }
                ;;
            *) print_fail "无效选项"; return 1 ;;
        esac
    else
        print_info "已存在镜像 $DEFAULT_IMAGE — 复用"
    fi

    # 4. backup before any container churn
    local b
    b=$(backup_now)
    print_info "Pre-install 备份: $b"

    # 5. start
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    print_step "启动容器 (restart=$RESTART_POLICY)"
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        print_fail "docker run 失败"
        return 1
    fi
    print_ok "容器已启动: $NAME"

    # 6. verify
    sleep 3
    cmd_verify || print_warn "verify 有未通过项，请查看上方输出"
}

cmd_update() {
    parse_flags "$@"
    reset_precheck_counters
    basic_precheck update
    (( PRECHECK_FAIL > 0 )) && return 1
    ensure_dependencies || return 1
    reset_precheck_counters
    docker_precheck
    (( PRECHECK_FAIL > 0 )) && return 1

    if ! container_exists; then
        print_warn "容器不存在；切换到 install 流程"
        cmd_install "$@"
        return $?
    fi

    local backup_dir previous_image
    backup_dir=$(backup_now)
    print_ok "升级前已备份: $backup_dir"
    previous_image=$(current_image_id)

    print_info "升级镜像来源:"
    print_choice "1)" "从 GitHub 拉源码并本地构建 (与最新 commit 对齐)"
    print_choice "2)" "使用当前目录源码构建"
    print_choice "3)" "拉取远程镜像"
    print_choice "4)" "跳过镜像更新（仅重新创建容器）"
    local choice img d
    choice=$(prompt_read "请选择" "1")
    case "$choice" in
        1)
            d=$(fetch_or_use_source) || return 1
            print_cmd "docker build -t $DEFAULT_IMAGE ."
            ( cd "$d" && docker build -t "$DEFAULT_IMAGE" . ) || { print_fail "build 失败"; return 1; }
            ;;
        2)
            [[ -z "$SOURCE_DIR" ]] && { print_fail "未在源码目录运行"; return 1; }
            print_cmd "docker build -t $DEFAULT_IMAGE ."
            ( cd "$SOURCE_DIR" && docker build -t "$DEFAULT_IMAGE" . ) || { print_fail "build 失败"; return 1; }
            ;;
        3)
            img=$(prompt_read "镜像名" "$DEFAULT_IMAGE")
            print_cmd "docker pull $img"
            docker pull "$img" || { print_fail "pull 失败"; return 1; }
            [[ "$img" != "$DEFAULT_IMAGE" ]] && { print_cmd "docker tag $img $DEFAULT_IMAGE"; docker tag "$img" "$DEFAULT_IMAGE"; }
            ;;
        4) print_info "跳过镜像更新" ;;
        *) print_fail "无效选项"; return 1 ;;
    esac

    print_step "停止旧容器"
    print_cmd "docker stop $NAME && docker rm $NAME"
    docker stop "$NAME" >/dev/null 2>&1 || true
    docker rm   "$NAME" >/dev/null 2>&1 || true

    print_step "启动新容器 (restart=$RESTART_POLICY)"
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        print_warn "新容器启动失败，触发自动回滚"
        _auto_rollback "$backup_dir" "$previous_image"
        return 1
    fi
    sleep 4
    if ! verify_basic; then
        print_warn "verify L1 失败，触发自动回滚"
        _auto_rollback "$backup_dir" "$previous_image"
        return 1
    fi
    print_ok "升级完成"
    cmd_verify || print_warn "verify 有未通过项，请查看上方输出"
}

_auto_rollback() {
    local backup_dir="$1" prev="$2"
    print_step "Auto rollback → $backup_dir"
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    [[ -f "$backup_dir/config.json"   ]] && cp -p "$backup_dir/config.json" "$CONFIG_FILE"
    [[ -f "$backup_dir/certs.tar.gz"  ]] && tar -C "$CONFIG_DIR" -xzf "$backup_dir/certs.tar.gz" 2>/dev/null || true
    local image_to_use="$prev"
    if [[ -z "$image_to_use" || "$image_to_use" == "<no value>" ]]; then
        [[ -f "$backup_dir/image-name.txt" ]] && image_to_use="$(<"$backup_dir/image-name.txt")"
    fi
    [[ -z "$image_to_use" ]] && image_to_use="$DEFAULT_IMAGE"
    print_info "rollback image: $image_to_use"
    if _docker_run "$image_to_use" "always"; then
        sleep 3
        if verify_basic; then
            print_ok "回滚成功"
            return 0
        fi
    fi
    print_fail "回滚启动失败 — 容器状态请用 yunzes-node status / docker logs $NAME 查看"
    return 1
}

cmd_start() {
    if container_exists; then
        if container_running; then
            print_info "$NAME 已经在运行"
        else
            print_cmd "docker start $NAME"
            docker start "$NAME" >/dev/null && print_ok "已启动" || print_fail "启动失败"
        fi
    else
        print_warn "容器不存在；执行 yunzes-node install 先安装"
    fi
}

cmd_stop() {
    if container_running; then
        print_cmd "docker stop $NAME"
        docker stop "$NAME" >/dev/null && print_ok "已停止"
    else
        print_info "$NAME 未在运行"
    fi
}

cmd_restart() {
    if container_exists; then
        print_cmd "docker restart $NAME"
        docker restart "$NAME" >/dev/null && print_ok "已重启"
    else
        print_warn "容器不存在"
    fi
}

cmd_redeploy() {
    parse_flags "$@"
    if ! container_exists; then
        print_warn "容器不存在；切换到 install"
        cmd_install "$@"
        return $?
    fi
    local backup_dir
    backup_dir=$(backup_now)
    print_ok "重新部署前备份: $backup_dir"
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        print_fail "启动失败，自动回滚"
        _auto_rollback "$backup_dir" "$DEFAULT_IMAGE"
        return 1
    fi
    sleep 3
    cmd_verify || print_warn "verify 有未通过项"
}

cmd_status() {
    if ! container_exists; then
        print_info "$NAME 容器不存在"
        return 0
    fi
    print_info "以下为 docker ps 原始输出:"
    docker ps -a --filter "name=^${NAME}$" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Ports}}"
    echo
    print_info "以下为 docker stats 原始输出:"
    docker stats --no-stream "$NAME" 2>/dev/null || true
}

cmd_logs() {
    local n="${1:-100}"
    case "$n" in
        100|300) ;;
        ''|*[!0-9]*) n=100 ;;
    esac
    print_info "以下为 docker logs --tail $n 原始输出:"
    docker logs --tail "$n" "$NAME" 2>&1 || print_warn "容器不存在"
}

cmd_follow_log() {
    print_info "跟随 docker logs (Ctrl-C 退出):"
    docker logs -f "$NAME" 2>&1 || print_warn "容器不存在"
}

cmd_edit_config() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        print_warn "$CONFIG_FILE 不存在；先用 yunzes-node gen-config 生成"
        return 1
    fi
    local editor="${EDITOR:-}"
    if [[ -z "$editor" ]]; then
        if   command -v nano >/dev/null 2>&1; then editor=nano
        elif command -v vi   >/dev/null 2>&1; then editor=vi
        else print_fail "未找到 nano/vi，请设置 EDITOR 环境变量"; return 1
        fi
    fi
    local backup
    backup=$(backup_now)
    print_info "已备份当前配置: $backup"
    cp -p "$CONFIG_FILE" "${CONFIG_FILE}.bak"
    print_cmd "$editor $CONFIG_FILE"
    "$editor" "$CONFIG_FILE"
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        print_fail "保存的 config.json 非合法 JSON; 恢复 .bak"
        mv "${CONFIG_FILE}.bak" "$CONFIG_FILE"
        return 1
    fi
    rm -f "${CONFIG_FILE}.bak"
    print_ok "JSON 校验通过"
    if container_running && confirm "立即重启容器使配置生效" y; then
        cmd_restart
    fi
}

cmd_show_config() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        print_warn "$CONFIG_FILE 不存在"
        return 1
    fi
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        print_fail "config.json 非合法 JSON"; return 1
    fi
    print_info "以下为 config.json 内容，ApiKey 已隐藏:"
    jq '
        .Nodes |= (map(
            if has("ApiKey") and (.ApiKey | type) == "string" and (.ApiKey | length) > 0 then
                .ApiKey = (.ApiKey[0:4] + "********" + .ApiKey[-4:])
            else . end
        ))
    ' "$CONFIG_FILE"
}

gen_config_template() {
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
    print_ok "已写入模板: $dest"
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
    print_info "进入交互式配置生成 — Ctrl-C 中途退出会保留模板状态"
    echo
    local nodes_json="[]"
    while true; do
        local idx
        idx=$(echo "$nodes_json" | jq 'length')
        print_title "--- 添加节点 #${idx} ---"
        local api_host api_key node_id node_type listen_ip timeout
        local cert_mode cert_domain cert_file key_file email
        api_host=$(prompt_read "ApiHost"      "https://your-panel.example.com")
        api_key=$(prompt_read  "ApiKey"       "")
        node_id=$(prompt_read  "NodeID (整数)" "1")
        print_info "支持协议: vless / vmess / trojan / shadowsocks / hysteria2 / tuic / anytls"
        node_type=$(prompt_read "NodeType"     "vless")
        listen_ip=$(prompt_read "ListenIP"     "0.0.0.0")
        timeout=$(prompt_read   "Timeout (秒)" "30")

        local need_tls=0
        case "$node_type" in
            hysteria2|tuic|anytls) need_tls=1 ;;
            vless|vmess|trojan)
                if confirm "该协议是否启用 TLS (reality / 无加密回答 N)" y; then
                    need_tls=1
                fi
                ;;
        esac

        local cert_obj="null"
        if (( need_tls )); then
            print_info "CertMode 选项: http (ACME HTTP-01) / dns (ACME DNS-01) / file (你提供) / self (自签) / none"
            cert_mode=$(prompt_read "CertMode" "self")
            case "$cert_mode" in
                http|dns|file|self)
                    cert_domain=$(prompt_read "CertDomain"          "")
                    cert_file=$(prompt_read   "CertFile"  "/etc/yunzes-node/certs/${node_type}${node_id}.crt")
                    key_file=$(prompt_read    "KeyFile"   "/etc/yunzes-node/certs/${node_type}${node_id}.key")
                    if [[ "$cert_mode" =~ ^(http|dns)$ ]]; then
                        email=$(prompt_read "Email (ACME 注册用)" "")
                    else
                        email=""
                    fi
                    cert_obj=$(jq -n \
                        --arg m "$cert_mode" --arg d "$cert_domain" \
                        --arg cf "$cert_file" --arg kf "$key_file" --arg e "$email" \
                        '{CertMode:$m, CertDomain:$d, CertFile:$cf, KeyFile:$kf, Email:$e, RenewBeforeDays:30}')
                    ;;
                none|"") cert_obj='{"CertMode":"none"}' ;;
                *) print_warn "未知 CertMode '$cert_mode'，回退到 none"; cert_obj='{"CertMode":"none"}' ;;
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
        print_ok "已写入 $CONFIG_FILE"
    else
        print_fail "生成的 JSON 不合法 (脚本 bug, 请反馈)"
        return 1
    fi
}

cmd_check_panel() {
    [[ -f "$CONFIG_FILE" ]] || { print_fail "无 config.json"; return 1; }
    jq empty "$CONFIG_FILE" 2>/dev/null || { print_fail "config.json 非合法 JSON"; return 1; }
    local total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE")
    local idx host node_id ntype path code
    for ((idx=0; idx<total; idx++)); do
        host=$(jq -r ".Nodes[$idx].ApiHost"  "$CONFIG_FILE")
        node_id=$(jq -r ".Nodes[$idx].NodeID" "$CONFIG_FILE")
        ntype=$(jq -r  ".Nodes[$idx].NodeType" "$CONFIG_FILE")
        path="${host%/}/v1/server/config?protocol=${ntype}&server_id=${node_id}"
        print_step "节点 #$idx → $path"
        print_cmd  "curl -s --connect-timeout 5 $path"
        code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$path" || true)
        case "$code" in
            200) print_ok   "HTTP 200 — panel 返回节点配置" ;;
            401|403) print_warn "HTTP $code — panel 拒绝 (ApiKey 可能错或权限不足)" ;;
            404) print_warn "HTTP 404 — 节点 ID $node_id 不存在或路径不对" ;;
            000|"") print_fail "无法连接 $host" ;;
            *) print_info "HTTP $code (具体含义看 panel 文档)" ;;
        esac
    done
}

cmd_ports() {
    if ! command -v ss >/dev/null 2>&1; then
        print_fail "缺少 ss 命令; apt install -y iproute2"
        return 1
    fi
    if container_running; then
        local pid
        pid=$(docker inspect --format '{{.State.Pid}}' "$NAME" 2>/dev/null || true)
        print_info "yunzes-node PID = $pid (host network 下与宿主端口表合并)"
        print_info "以下为 ss -lntup 过滤后的原始输出:"
        ss -lntup | awk -v p="$pid" 'NR==1 || $0 ~ p'
    else
        print_warn "容器未运行 — 显示宿主当前所有监听"
        ss -lntup
    fi
}

cmd_containers() {
    print_info "以下为 docker ps 原始输出:"
    docker ps -a --filter "name=^${NAME}$" --no-trunc
    echo
    print_info "以下为 docker inspect 头 80 行原始输出:"
    docker inspect "$NAME" 2>/dev/null | head -80 || print_warn "容器不存在"
}

cmd_backup() {
    ensure_dirs
    local b
    b=$(backup_now)
    print_ok "备份完成: $b"
    print_info "以下为 ls -lh 原始输出:"
    ls -lh "$b"
    echo
    print_info "回滚命令: yunzes-node rollback (或菜单 18)"
}

cmd_rollback() {
    local backups
    mapfile -t backups < <(list_backups)
    if (( ${#backups[@]} == 0 )); then
        print_warn "没有可用的备份"
        return 0
    fi
    print_info "可回滚的备份:"
    local i
    for ((i=0; i<${#backups[@]}; i++)); do
        print_choice "$((i+1)))" "${backups[$i]}"
    done
    print_choice "q)" "取消"
    local choice
    choice=$(prompt_read "选择 [1-${#backups[@]}, q]" "q")
    [[ "$choice" =~ ^[Qq]$ ]] && { print_info "已取消"; return 0; }
    if ! [[ "$choice" =~ ^[0-9]+$ ]] || (( choice < 1 || choice > ${#backups[@]} )); then
        print_fail "无效选项"; return 1
    fi
    local target="${backups[$((choice-1))]}"
    print_danger "回滚到 $target — 将覆盖当前 config.json + certs/ + 容器"
    print_warn   "此操作不可直接撤销, 但回滚前会自动再做一次安全备份"
    if ! confirm "继续" n; then
        print_info "已取消"; return 0
    fi
    local before
    before=$(backup_now)
    print_info "当前状态已备份到 $before (rollback-before-保险)"
    if restore_from_backup "$target"; then
        sleep 3
        cmd_verify || print_warn "verify 有未通过项"
    else
        print_fail "回滚失败 — yunzes-node status 查看容器; $before 是回滚前的备份"
        return 1
    fi
}

cmd_cleanup_images() {
    print_info "悬空镜像 (dangling):"
    docker images --filter "dangling=true" --format "table {{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}"
    echo
    print_danger "下面的清理操作会删除所有 dangling 镜像 (容器进行中的镜像不会被影响)"
    if confirm "继续清理 dangling 镜像" n; then
        print_cmd "docker image prune -f"
        docker image prune -f
        print_ok "清理完成"
    fi
    echo
    print_info "全部 yunzes-node 历史镜像 (含 untagged):"
    docker images --filter "reference=yunzes-node" --format "table {{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}"
}

# -----------------------------------------------------------------------------
# Fake panel 4-protocol test
# -----------------------------------------------------------------------------
write_fake_panel_py() {
    cat > "$FAKE_PANEL_FILE" <<'PYEOF'
#!/usr/bin/env python3
"""Fake panel for yunzes-node integration testing."""
import json, sys
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
        print_info "fake panel 已在运行 (PID $(<"$FAKE_PANEL_PID_FILE"))"
        return 0
    fi
    write_fake_panel_py
    print_cmd "python3 $FAKE_PANEL_FILE > $FAKE_PANEL_LOG_FILE 2>&1 &"
    nohup python3 "$FAKE_PANEL_FILE" >"$FAKE_PANEL_LOG_FILE" 2>&1 &
    echo $! > "$FAKE_PANEL_PID_FILE"
    sleep 1
    if curl -s --max-time 2 "http://127.0.0.1:${FAKE_PANEL_PORT}/v1/server/user" >/dev/null; then
        print_ok "fake panel 启动成功 (PID $(<"$FAKE_PANEL_PID_FILE"))"
    else
        print_fail "fake panel 启动失败 — 查看 $FAKE_PANEL_LOG_FILE"
        return 1
    fi
}

write_fake_test_config() {
    # Use a separate FAKE_CERTS_DIR so test certs do not collide with real
    # /etc/yunzes-node/certs claims (C3 claimCertFiles forbids two distinct
    # CertDomains pointing at the same file). FAKE_CERTS_DIR is created
    # inside the host bind-mount so the container can write here too.
    cat > "$FAKE_TEST_CONFIG" <<JSON
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
                "CertFile":   "${FAKE_CERTS_DIR}/vless101.crt",
                "KeyFile":    "${FAKE_CERTS_DIR}/vless101.key",
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
                "CertFile":   "${FAKE_CERTS_DIR}/hy2103.crt",
                "KeyFile":    "${FAKE_CERTS_DIR}/hy2103.key",
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
    # Default for fake-test: --restart no, regardless of --no-restart flag,
    # so a panic doesn't trigger restart loops that pollute logs.
    local effective_restart="no"

    if ! command -v python3 >/dev/null 2>&1; then
        print_fail "缺少 python3 — apt install -y python3"
        return 1
    fi
    if ! command -v curl >/dev/null 2>&1; then
        print_fail "缺少 curl"; return 1
    fi
    detect_docker_state >/dev/null
    case $? in
        0) ;;
        10) print_fail "Docker 未安装"; return 1 ;;
        11) print_fail "Docker daemon 未运行"; return 1 ;;
        12) print_fail "无 docker socket 权限"; return 1 ;;
    esac
    if ! docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        print_fail "镜像 $DEFAULT_IMAGE 不存在; 先 yunzes-node install 或 docker build"
        return 1
    fi

    ensure_dirs
    mkdir -p "$FAKE_CERTS_DIR"
    chmod 750 "$FAKE_CERTS_DIR" 2>/dev/null || true

    # Save original config; track exact backup dir for the cleanup phase.
    local pre_backup=""
    if [[ -f "$CONFIG_FILE" ]]; then
        pre_backup=$(backup_now)
        print_info "fake-test 前已备份原配置: $pre_backup"
    fi
    write_fake_test_config
    cp -f "$FAKE_TEST_CONFIG" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    print_ok "已写入 4 协议测试配置 → $CONFIG_FILE"

    start_fake_panel || return 1

    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    print_step "启动测试容器 (restart=$effective_restart)"
    if ! _docker_run "$DEFAULT_IMAGE" "$effective_restart"; then
        print_fail "测试容器启动失败"
        # Restore prior config on early failure.
        [[ -n "$pre_backup" && -f "$pre_backup/config.json" ]] \
            && cp -p "$pre_backup/config.json" "$CONFIG_FILE"
        return 1
    fi

    print_step "等待 5 秒..."; sleep 5
    echo

    local logs fake_pass=0 fake_fail=0
    logs="$(docker logs "$NAME" 2>&1 || true)"

    local _bad
    _bad=$(echo "$logs" | grep -E -i 'panic|nil pointer|runtime error|segmentation violation' | head -3 || true)
    if [[ -z "$_bad" ]]; then
        print_ok "无 panic / nil pointer / runtime error"; fake_pass=$((fake_pass+1))
    else
        print_fail "发现致命错误:"
        print_info "以下为匹配到的原始日志行:"
        echo "$_bad" | sed 's/^/      /'
        fake_fail=$((fake_fail+1))
    fi
    local marker
    for marker in "Core Selector" "Adding node inbound" "logical_tag" "core=" "runtime_key" "protocol=" "server_id" "port="; do
        if echo "$logs" | grep -qF "$marker"; then
            print_ok "日志含字段: $marker"; fake_pass=$((fake_pass+1))
        else
            print_fail "日志未见: $marker"; fake_fail=$((fake_fail+1))
        fi
    done

    if command -v ss >/dev/null 2>&1; then
        echo
        print_step "端口监听检查 (host network)"
        local check port proto hits
        for check in "8101 tcp" "8102 tcp" "8102 udp" "8103 udp" "8104 tcp"; do
            read -r port proto <<<"$check"
            case "$proto" in
                tcp) hits=$(ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
                udp) hits=$(ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
            esac
            if [[ -n "$hits" ]] && echo "$hits" | grep -q yunzes-node; then
                print_ok "$port/$proto 由 yunzes-node 监听"; fake_pass=$((fake_pass+1))
            else
                print_fail "$port/$proto 未由 yunzes-node 监听"; fake_fail=$((fake_fail+1))
            fi
        done
    else
        print_warn "缺少 ss, 跳过端口监听检查"
    fi

    echo
    print_step "证书复用检查 (restart 后应 reuse)"
    print_cmd "docker restart $NAME"
    docker restart "$NAME" >/dev/null 2>&1 || true
    sleep 3
    local cert_lines
    cert_lines=$(docker logs --tail 200 "$NAME" 2>&1 | grep cert_action || true)
    if echo "$cert_lines" | grep -q 'cert_action=reuse'; then
        print_ok "重启后 cert_action=reuse (C3 持久化生效)"; fake_pass=$((fake_pass+1))
    else
        print_warn "未在重启日志里找到 cert_action=reuse"
        print_info "以下为 cert_action 相关日志原文:"
        echo "$cert_lines" | sed 's/^/      /'
    fi

    echo
    print_separator
    printf "fake-test 汇总: %bPASS=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$fake_pass" "${C_PLAIN}" "${C_RED}" "$fake_fail" "${C_PLAIN}"
    print_separator

    echo
    if confirm "停止 fake panel" y; then
        cmd_stop_fake_panel
    fi
    if confirm "删除测试容器 ($NAME)" n; then
        print_cmd "docker rm -f $NAME"
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        print_ok "测试容器已删除"
    fi
    if confirm "保留测试证书 ($FAKE_CERTS_DIR)" n; then
        print_info "保留 — 测试证书隔离在 $FAKE_CERTS_DIR, 不影响真实 $CERTS_DIR"
    else
        print_cmd "rm -rf $FAKE_CERTS_DIR"
        rm -rf "$FAKE_CERTS_DIR"
        print_ok "测试证书目录已删除"
    fi
    echo
    if [[ -n "$pre_backup" ]]; then
        if confirm "恢复 fake-test 之前的 config.json (来自 $pre_backup)" y; then
            if [[ -f "$pre_backup/config.json" ]]; then
                cp -p "$pre_backup/config.json" "$CONFIG_FILE"
                print_ok "已恢复: $pre_backup/config.json"
            else
                print_warn "$pre_backup/config.json 不存在; 保留测试 config.json"
            fi
        fi
    else
        print_info "fake-test 启动前没有 config.json, 无需恢复"
    fi
    return $fake_fail
}

cmd_stop_fake_panel() {
    if [[ -f "$FAKE_PANEL_PID_FILE" ]]; then
        local pid
        pid=$(<"$FAKE_PANEL_PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            print_cmd "kill $pid"
            kill "$pid" 2>/dev/null && print_ok "fake panel 已停止 (PID $pid)"
        fi
        rm -f "$FAKE_PANEL_PID_FILE"
    fi
    pkill -f fake_panel.py 2>/dev/null || true
}

# -----------------------------------------------------------------------------
# Uninstall
# -----------------------------------------------------------------------------
cmd_uninstall() {
    print_danger "卸载操作: 将删除容器并可选删除镜像 / 全局命令"
    print_warn   "保留: $CONFIG_DIR (含证书) 与 $BACKUP_DIR (备份)"
    if ! confirm "继续" y; then
        print_info "已取消"
        return 0
    fi
    if container_exists; then
        print_cmd "docker rm -f $NAME"
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        print_ok "容器已删除"
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        if confirm "同时删除镜像 $DEFAULT_IMAGE" n; then
            print_cmd "docker rmi -f $DEFAULT_IMAGE"
            docker rmi -f "$DEFAULT_IMAGE" >/dev/null 2>&1 && print_ok "镜像已删除"
        fi
    fi
    if [[ -f "$INSTALLED_PATH" ]] && confirm "删除全局命令 $INSTALLED_PATH" n; then
        rm -f "$INSTALLED_PATH"
        print_ok "$INSTALLED_PATH 已删除"
    fi
    print_info "保留: $CONFIG_DIR  $CERTS_DIR  $BACKUP_DIR"
    print_info "如需彻底清理: yunzes-node uninstall-full"
}

cmd_uninstall_full() {
    print_danger "危险操作: 完全卸载 — 将删除以下所有内容"
    print_fail   "  - 容器        $NAME"
    print_fail   "  - 镜像        $DEFAULT_IMAGE"
    print_fail   "  - 配置目录    $CONFIG_DIR (含证书 + fake-test 证书)"
    print_fail   "  - 运行目录    $RUN_DIR (含全部备份 + 源码 $SRC_DIR)"
    print_fail   "  - 命令入口    $INSTALLED_PATH"
    print_warn   "此操作不可恢复"
    print_fix    "如需取消, 直接回车或输入除 'DELETE YUNZES NODE' 之外的任何文字"
    echo
    if ! confirm_phrase "请输入 DELETE YUNZES NODE 二次确认" "DELETE YUNZES NODE"; then
        print_info "已取消"
        return 0
    fi
    cmd_stop_fake_panel
    if container_exists; then
        print_cmd "docker rm -f $NAME"
        docker rm -f "$NAME" >/dev/null 2>&1 || true
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        print_cmd "docker rmi -f $DEFAULT_IMAGE"
        docker rmi -f "$DEFAULT_IMAGE" >/dev/null 2>&1 || true
    fi
    print_cmd "rm -rf $CONFIG_DIR $RUN_DIR $INSTALLED_PATH"
    rm -rf "$CONFIG_DIR" "$RUN_DIR"
    rm -f "$INSTALLED_PATH"
    print_ok "彻底卸载完成"
}

cmd_setup_entry() {
    if ! is_root; then
        print_fail "需 root 才能写 $INSTALLED_PATH"
        return 1
    fi
    if [[ ! -f "$SCRIPT_PATH" ]]; then
        print_fail "找不到当前脚本路径: $SCRIPT_PATH"
        return 1
    fi
    print_cmd "install -m 0755 $SCRIPT_PATH $INSTALLED_PATH"
    install -m 0755 "$SCRIPT_PATH" "$INSTALLED_PATH"
    print_ok "命令已安装 / 更新: $INSTALLED_PATH"
    print_info "现在直接 yunzes-node 即可进入菜单"
}

# -----------------------------------------------------------------------------
# Top-level dispatcher
# -----------------------------------------------------------------------------
usage() {
    print_title "yunzes-node v${SCRIPT_VERSION}  —  单容器双核心 Docker 部署"
    print_separator
    cat <<EOF

用法:
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

环境变量:
    NO_COLOR=1                      关闭彩色输出 (适合日志采集 / CI)

路径约定:
    配置  ${CONFIG_FILE}
    证书  ${CERTS_DIR}
    fake  ${FAKE_CERTS_DIR}
    备份  ${BACKUP_DIR}
    源码  ${SRC_DIR}
    日志  docker logs ${NAME}

详见 README.md。
EOF
}

main() {
    SOURCE_DIR="$(detect_source_dir 2>/dev/null || true)"

    local cmd="${1:-menu}"
    [[ $# -gt 0 ]] && shift

    case "$cmd" in
        install|update|upgrade|redeploy|edit-config|gen-config|backup|rollback|fake-test|uninstall|uninstall-full|setup-entry)
            if ! is_root; then
                print_fail "请使用 root 运行: sudo $SCRIPT_PATH $cmd $*"
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
        version|-v|--version) print_info "yunzes-node script v${SCRIPT_VERSION}" ;;
        help|-h|--help)  usage ;;
        *)               usage; exit 1 ;;
    esac
}

main "$@"
