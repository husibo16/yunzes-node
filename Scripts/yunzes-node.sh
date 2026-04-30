#!/usr/bin/env bash
# ============================================================================
# yunzes-node — single-file modular management script.
# ============================================================================
#
# Production entry-point: install once via this script and the global
# `yunzes-node` command becomes the operator's daily driver. Bare invocation
# launches the interactive menu; every menu item also has a non-interactive
# subcommand (`yunzes-node install`, `yunzes-node verify`, ...).
#
# Default deployment is always Docker + host network + single container —
# never split xray/sing-box into separate containers. The Go binary inside
# the image already links both runtimes (C0-C5 refactor).
#
# ----------------------------------------------------------------------------
# MODULE MAP (top -> bottom; bash sources the file linearly)
# ----------------------------------------------------------------------------
#   M01 core         constants, paths, locale resolution, color palette
#   M02 output       print_step/info/ok/warn/fail/fix/cmd/danger/title
#   M03 prompt       confirm, confirm_phrase, prompt_read, prompt_required
#   M04 detect       OS, arch, source-tree, docker daemon state, container ps
#   M05 flags        parse_flags (--no-restart) for non-interactive subcommands
#   M06 banner       ASCII banner + 25-item interactive menu + lang picker
#   M07 precheck     basic / dependency / docker / file phases + summary
#   M08 dependency   apt-driven ensure_dependencies + dockerd start
#   M09 backup       backup_now / list_backups / restore_from_backup
#   M10 source       fetch_or_use_source (git clone or pull from REPO_URL)
#   M11 container    _docker_run, ensure_dirs (canonical run-line wrapper)
#   M12 verify       L1 basic / L2 network (auto-diag) / L3 business
#   M13 panel-probe  preflight_panel_check (v1 per-node OR v2 smart probe)
#   M14 validate     validate_config (NodeID dup, CertDomain non-empty, ...)
#   M15 config-gen   smart-mode (/v2) -> v1-probe fallback -> manual flow
#   M16 install      cmd_install / cmd_update / cmd_redeploy + auto-rollback
#   M17 lifecycle    cmd_start / stop / restart / status
#   M18 logs         cmd_logs / cmd_follow_log
#   M19 config-mgmt  cmd_edit_config / cmd_show_config / cmd_gen_config
#   M20 panel-tools  cmd_check_panel / cmd_ports / cmd_containers / cmd_backup /
#                    cmd_rollback / cmd_cleanup_images
#   M21 fake-panel   cmd_fake_test (4-protocol probe) + cmd_stop_fake_panel
#   M22 cleanup      cmd_uninstall / cmd_uninstall_full / cmd_setup_entry
#   M23 lang         cmd_lang (persistent zh/en switch)
#   M24 dispatch     usage / main (top-level argument parser)
#
# ----------------------------------------------------------------------------
# Cross-module conventions
# ----------------------------------------------------------------------------
#   - Locale: every operator-visible string flows through _t "zh" "en".
#     Default zh; switch via `yunzes-node lang en` or YUNZES_LANG=en. The
#     script does NOT consult $LANG (most VPS images carry en_US.UTF-8 by
#     default, which is not a preference signal).
#   - Color: all output goes through print_* helpers (M02). Colors auto-
#     disable on non-TTY stdout or NO_COLOR=1.
#   - Errors: [FAIL] for hard errors, [WARN] for advisories, [FIX ] for
#     one-line remediation hints, [CMD ] before exec.
#   - Subcommands: every cmd_X is reachable both from the menu (M06) and
#     as `yunzes-node X` via the M24 dispatcher.
#   - State files: /opt/yunzes-node/state/ chmod 700.
#
# Required system commands (M08 ensure_dependencies installs missing on
# Debian/Ubuntu): docker, curl, jq, tar, git, ss (iproute2), python3 (only
# for fake-test).
# ============================================================================

set -uo pipefail
IFS=$'\n\t'

# ============================================================================
# M01 core: constants, paths, locale, color palette
# ============================================================================

readonly SCRIPT_VERSION="2.0.1"
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
# Locale resolution order (first match wins):
#   1. $YUNZES_LANG (explicit, one-shot override)
#   2. /opt/yunzes-node/state/locale (persistent operator choice via
#      `yunzes-node lang zh|en`)
#   3. default zh
#
# We intentionally DO NOT consult $LANG here — most VPS images ship with
# LANG=en_US.UTF-8 by default, which has nothing to do with the operator's
# language preference. Use `yunzes-node lang en` to opt in.
# -----------------------------------------------------------------------------
LOCALE_STATE_FILE="/opt/yunzes-node/state/locale"
if [[ -n "${YUNZES_LANG:-}" ]]; then
    case "${YUNZES_LANG,,}" in
        zh*) LOCALE=zh ;;
        en*) LOCALE=en ;;
        *)   LOCALE=zh ;;
    esac
elif [[ -r "$LOCALE_STATE_FILE" ]]; then
    case "$(tr -d '[:space:]' < "$LOCALE_STATE_FILE" 2>/dev/null)" in
        zh) LOCALE=zh ;;
        en) LOCALE=en ;;
        *)  LOCALE=zh ;;
    esac
else
    LOCALE=zh
fi
# LOCALE is intentionally NOT readonly so the first-run language picker
# (cmd_menu) can persist a new choice and re-exec to pick it up.

# _t "zh-text" "en-text" — emit the text matching $LOCALE, no trailing
# newline. Both arguments are required so a maintainer always sees both
# translations side-by-side at the call site.
_t() {
    if [[ "$LOCALE" == "zh" ]]; then printf '%s' "$1"; else printf '%s' "${2:-$1}"; fi
}

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
    readonly C_RED="" C_GREEN="" C_YELLOW="" C_BLUE="" C_MAGENTA=""
    readonly C_CYAN="" C_GRAY="" C_BOLD="" C_BOLD_CYAN="" C_BOLD_RED=""
    readonly C_BOLD_YELLOW="" C_DIM="" C_PLAIN=""
fi

PRECHECK_PASS=0
PRECHECK_WARN=0
PRECHECK_FAIL=0
NO_RESTART=0
RESTART_POLICY="always"

SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null || echo "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(dirname "$SCRIPT_PATH")"
SOURCE_DIR=""

# ============================================================================
# M02 output: print_* helpers — every operator-facing line flows through one.
# ============================================================================
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
    printf "  %b%s%b %b%s%b\n" "${C_GREEN}" "$1" "${C_PLAIN}" "${C_CYAN}" "$2" "${C_PLAIN}"
}

info()    { print_info    "$@"; }
ok()      { print_success "$@"; }
warn()    { print_warn    "$@"; }
fail()    { print_error   "$@"; }
step()    { print_step    "$@"; }
fix_hint(){ print_fix     "$@"; }

err_exit() { fail "$*"; exit 1; }

# ============================================================================
# M03 prompt: interactive helpers — colored bilingual prompts with cancel.
# ============================================================================
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

prompt_required() {
    # prompt_required "question" "error-msg-zh" "error-msg-en"
    # Loops until the user enters a non-empty value OR types q/cancel/取消.
    # Returns 0 with the value on stdout, or 1 if the user explicitly cancels
    # so callers can fall back gracefully (e.g. switch CertMode to "none").
    local q="$1" err_zh="$2" err_en="$3" reply
    while true; do
        printf "%b? %s %b(q 取消 / cancel)%b: %b" \
            "${C_CYAN}" "$q" "${C_DIM}" "${C_CYAN}" "${C_PLAIN}" >&2
        read -r reply || return 1
        case "$reply" in
            q|Q|quit|exit|cancel|取消)
                return 1 ;;
            "")
                print_warn "$(_t "$err_zh" "$err_en")"
                ;;
            *)
                echo "$reply"
                return 0
                ;;
        esac
    done
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

# ============================================================================
# M04 detect: OS, arch, source-tree, docker daemon state, container ps.
# ============================================================================
is_root() { [[ $EUID -eq 0 ]]; }

# detect_os: grep+cut /etc/os-release. Sourcing was unsafe because some
# distros mark $NAME / $VERSION readonly, which abort the shell on `source`.
detect_os() {
    local id id_like
    id=$(grep -E '^ID=' /etc/os-release 2>/dev/null \
            | head -1 | cut -d= -f2- | tr -d '"' || true)
    id_like=$(grep -E '^ID_LIKE=' /etc/os-release 2>/dev/null \
            | head -1 | cut -d= -f2- | tr -d '"' || true)
    case "$id" in debian) echo debian; return ;; ubuntu) echo ubuntu; return ;; esac
    case "$id_like" in
        *debian*) echo debian; return ;;
        *ubuntu*) echo ubuntu; return ;;
    esac
    echo other
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   echo amd64 ;;
        aarch64|arm64)  echo arm64 ;;
        *)              echo "$(uname -m)" ;;
    esac
}

detect_source_dir() {
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
#   0 ok / 10 not installed / 11 daemon down / 12 no socket permission
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

# ============================================================================
# M05 flags: parse_flags strips shared --no-restart from "$@".
# ============================================================================
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

# ============================================================================
# M06 banner: ASCII banner, 25-item menu, first-run language picker.
# ============================================================================
print_banner() {
    printf "%b" "${C_BOLD_CYAN}"
    cat <<'BANNER'

   __ __ _   _ _ _____  ___ ___    __ _  ___  ___  ___
   \ V / | | | | |_  / / __/ __|__/ /| || _ \| _ \| __|
    | || |_| |_| |/ /  \__ \__ \__/ /_| || _ /| _ /| _|
    |_||___\___/|_/___||___/___/_/  __\__|___||___||___|

BANNER
    printf "%b" "${C_PLAIN}"
    print_kv "$(_t "版本"     "version")"     "v${SCRIPT_VERSION}"
    print_kv "$(_t "镜像"     "image")"       "${DEFAULT_IMAGE}"
    print_kv "$(_t "配置目录" "config dir")"  "${CONFIG_DIR}"
    print_kv "$(_t "运行目录" "run dir")"     "${RUN_DIR}"
    print_kv "$(_t "语言"     "locale")"      "${LOCALE} ($(_t '中文' 'English'))"
    if [[ "$COLOR_ENABLED" == "0" ]]; then
        print_info "$(_t "颜色已禁用（NO_COLOR=1 或非交互式 stdout）" \
                        "Color disabled (NO_COLOR=1 or non-TTY stdout)")"
    fi
}

print_menu() {
    echo
    print_title  "$(_t '管理菜单' 'Management Menu')"
    print_separator
    print_menu_item  1 "$(_t '安装 yunzes-node'                      'Install yunzes-node')"
    print_menu_item  2 "$(_t '升级 yunzes-node'                      'Upgrade yunzes-node')"
    print_menu_item  3 "$(_t '启动 yunzes-node'                      'Start yunzes-node')"
    print_menu_item  4 "$(_t '停止 yunzes-node'                      'Stop yunzes-node')"
    print_menu_item  5 "$(_t '重启 yunzes-node'                      'Restart yunzes-node')"
    print_menu_item  6 "$(_t '重新部署容器'                          'Redeploy container')"
    print_menu_item  7 "$(_t '查看运行状态'                          'Show status')"
    print_menu_item  8 "$(_t '查看实时日志'                          'Follow logs')"
    print_menu_item  9 "$(_t '查看最近日志'                          'Show recent logs')"
    print_menu_item 10 "$(_t '验证节点服务'                          'Verify node service')"
    print_menu_item 11 "$(_t '编辑配置文件'                          'Edit config')"
    print_menu_item 12 "$(_t '查看当前配置 (隐藏 ApiKey)'            'Show config (ApiKey masked)')"
    print_menu_item 13 "$(_t '生成配置模板'                          'Generate config template')"
    print_menu_item 14 "$(_t '测试连接面板服务器'                    'Test panel API reachability')"
    print_menu_item 15 "$(_t '查看监听端口'                          'Show listening ports')"
    print_menu_item 16 "$(_t '查看 Docker 容器信息'                  'Show Docker container info')"
    print_menu_item 17 "$(_t '备份当前配置'                          'Backup current config')"
    print_menu_item 18 "$(_t '回滚到上一个备份'                      'Rollback to a backup')"
    print_menu_item 19 "$(_t '清理旧镜像'                            'Cleanup old images')"   danger
    print_menu_item 20 "$(_t '运行模拟面板四协议验证'                'Run fake-panel 4-protocol test')"
    print_menu_item 21 "$(_t '停止模拟面板'                          'Stop fake panel')"
    print_menu_item 22 "$(_t '卸载程序，保留配置和证书'              'Uninstall (keep config + certs)')"      danger
    print_menu_item 23 "$(_t '完全卸载（删除配置 + 证书）'           'Uninstall FULL (wipe everything)')"     danger
    print_menu_item 24 "$(_t "安装/更新命令入口 ${INSTALLED_PATH}"   "Install/update command entry ${INSTALLED_PATH}")"
    print_menu_item 25 "$(_t '退出' 'Exit')"   exit
    print_separator
}

# first_run_pick_language: prompt the operator the very first time they run
# the menu (no $YUNZES_LANG and no persistent state file). Writes the choice
# to LOCALE_STATE_FILE and reassigns $LOCALE in-place so subsequent _t calls
# (which read $LOCALE on every invocation) pick up the new value.
#
# We do NOT re-exec the script — exec'ing $SCRIPT_PATH directly fails when
# the operator invoked the script via `bash /path/to/yunzes-node.sh` and the
# file does not carry the execute bit. In-place reassign avoids the issue.
#
# Non-root operators can't persist (they can't write /opt), so they get a
# silent default-zh plus a single-line hint instead of an interactive prompt.
first_run_pick_language() {
    [[ -n "${YUNZES_LANG:-}" ]] && return 0
    [[ -f "$LOCALE_STATE_FILE" ]] && return 0
    if ! is_root; then
        printf "\n  %b首次运行：默认使用中文。 First run: defaulting to Chinese.%b\n" "${C_BOLD_CYAN}" "${C_PLAIN}"
        printf "  %bsudo yunzes-node lang en%b  %b切换到英文 / switch to English%b\n\n" "${C_GREEN}" "${C_PLAIN}" "${C_DIM}" "${C_PLAIN}"
        return 0
    fi
    printf "\n"
    printf "  %b请选择语言 / Please choose language%b\n" "${C_BOLD_CYAN}" "${C_PLAIN}"
    printf "    %b1)%b %b中文%b\n"   "${C_GREEN}" "${C_PLAIN}" "${C_CYAN}" "${C_PLAIN}"
    printf "    %b2)%b %bEnglish%b\n" "${C_GREEN}" "${C_PLAIN}" "${C_CYAN}" "${C_PLAIN}"
    printf "  %b? [1]: %b" "${C_CYAN}" "${C_PLAIN}"
    local choice
    read -r choice || true
    mkdir -p "$STATE_DIR" 2>/dev/null || true
    case "${choice:-1}" in
        2|en|english|English|英文)
            echo "en" > "$LOCALE_STATE_FILE" 2>/dev/null
            LOCALE=en
            ;;
        *)
            echo "zh" > "$LOCALE_STATE_FILE" 2>/dev/null
            LOCALE=zh
            ;;
    esac
}

cmd_menu() {
    first_run_pick_language
    while true; do
        clear || true
        print_banner
        print_menu
        local choice
        printf "%b%s%b" "${C_CYAN}" "$(_t '请选择 [1-25]: ' 'Select [1-25]: ')" "${C_PLAIN}"
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
            25|q|Q|exit) print_info "$(_t '再见。' 'Goodbye.')"; return 0 ;;
            *)  print_warn "$(_t "无效选项：$choice" "Invalid option: $choice")" ;;
        esac
        echo
        printf "%b%s%b" "${C_DIM}" "$(_t '按回车返回菜单...' 'Press Enter to return to menu...')" "${C_PLAIN}"
        read -r _ || true
    done
}

# ============================================================================
# M07 precheck: 4-stage pipeline (basic / dependency / docker / file)
# Each phase tallies into PRECHECK_PASS/WARN/FAIL via _pcok / _pcwarn / _pcfail.
# ============================================================================
_pcok()   { print_success "$1"; PRECHECK_PASS=$((PRECHECK_PASS+1)); }
_pcwarn() { print_warn    "$1"; PRECHECK_WARN=$((PRECHECK_WARN+1)); }
_pcfail() { print_error   "$1"; PRECHECK_FAIL=$((PRECHECK_FAIL+1)); }
reset_precheck_counters() { PRECHECK_PASS=0; PRECHECK_WARN=0; PRECHECK_FAIL=0; }

basic_precheck() {
    print_step "$(_t '预检: 基础环境' 'PreCheck: basic environment')"
    if is_root; then
        _pcok "$(_t '运行用户：root' 'Running as: root')"
    else
        _pcfail "$(_t "必须使用 root 运行（当前 UID=$EUID）" "Must run as root (current UID=$EUID)")"
        print_fix "$(_t "改用：sudo $SCRIPT_PATH ${1:-menu}" "Try: sudo $SCRIPT_PATH ${1:-menu}")"
    fi
    local os arch
    os=$(detect_os)
    case "$os" in
        debian|ubuntu) _pcok "$(_t "操作系统：$os" "OS: $os")" ;;
        *)             _pcwarn "$(_t "未在 Debian / Ubuntu 上测试（detected: $os）" "Untested on $os (Debian/Ubuntu recommended)")" ;;
    esac
    arch=$(detect_arch)
    case "$arch" in
        amd64|arm64) _pcok "$(_t "CPU 架构：$arch" "Arch: $arch")" ;;
        *)           _pcwarn "$(_t "未测试的 CPU 架构：$arch" "Untested CPU arch: $arch")" ;;
    esac
    if command -v free >/dev/null 2>&1; then
        local mem_free
        mem_free=$(free -h --si 2>/dev/null | awk '/^Mem:/{print $7}')
        [[ -n "$mem_free" ]] && _pcok "$(_t "可用内存：$mem_free" "Free memory: $mem_free")"
    fi
    local disk_free
    disk_free=$(df -h / 2>/dev/null | awk 'NR==2{print $4}')
    [[ -n "$disk_free" ]] && _pcok "$(_t "/ 可用磁盘：$disk_free" "/ free disk: $disk_free")"
}

DEPENDENCIES=(curl jq tar git ss python3)
DEP_PACKAGES=(curl jq tar git iproute2 python3)

dependency_precheck() {
    print_step "$(_t '预检: 命令行依赖' 'PreCheck: CLI dependencies')"
    local tool
    for tool in "${DEPENDENCIES[@]}"; do
        if command -v "$tool" >/dev/null 2>&1; then
            _pcok "$(_t "命令存在：$tool" "command available: $tool")"
        else
            _pcwarn "$(_t "缺少 $tool（ensure_dependencies 阶段会询问安装）" "missing $tool (ensure_dependencies will offer to install)")"
        fi
    done
}

# ============================================================================
# M08 dependency: ensure_dependencies — apt-driven install + dockerd start.
# Debian/Ubuntu only; other distros warn-and-skip and let docker_precheck
# do the hard fail.
# ============================================================================
ensure_dependencies() {
    local os
    os=$(detect_os)
    if [[ "$os" != "debian" && "$os" != "ubuntu" ]]; then
        print_warn "$(_t '非 Debian/Ubuntu 系统，跳过自动安装；请手动确保依赖已就绪' 'Not Debian/Ubuntu — skipping auto-install; ensure deps manually')"
        return 0
    fi
    print_step "$(_t '依赖检查与安装' 'Dependency check and install')"
    local missing=() i
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
        print_ok "$(_t '所有依赖已就绪' 'All dependencies present')"
    else
        print_info "$(_t "缺失依赖: ${missing[*]}" "Missing deps: ${missing[*]}")"
        if confirm "$(_t "执行 apt update && apt install -y ${missing[*]}" "Run apt update && apt install -y ${missing[*]}")" y; then
            print_cmd "apt-get update -y"
            DEBIAN_FRONTEND=noninteractive apt-get update -y || {
                print_fail "$(_t 'apt update 失败 — 请检查网络或 /etc/apt/sources.list' 'apt update failed — check network or /etc/apt/sources.list')"
                return 1
            }
            print_cmd "apt-get install -y ${missing[*]}"
            DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" || {
                print_fail "$(_t "apt install 失败 — 请手动 apt install -y ${missing[*]} 后重试" "apt install failed — run apt install -y ${missing[*]} manually then retry")"
                return 1
            }
            print_ok "$(_t '依赖安装完成' 'Dependencies installed')"
        else
            print_warn "$(_t '用户取消自动安装；请手动安装后重试' 'User declined auto-install; install manually then retry')"
            print_fix  "apt update && apt install -y ${missing[*]}"
            return 1
        fi
    fi

    if command -v docker >/dev/null 2>&1; then
        if ! docker info >/dev/null 2>&1; then
            print_step "$(_t '尝试启动 Docker daemon' 'Starting Docker daemon')"
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
                print_ok "$(_t 'Docker daemon 已启动' 'Docker daemon started')"
            else
                print_warn "$(_t 'Docker daemon 启动失败（可能需要手动 service docker start）' 'Docker daemon failed to start — try service docker start manually')"
            fi
        fi
    fi
    return 0
}

docker_precheck() {
    print_step "$(_t '预检: Docker' 'PreCheck: Docker')"
    detect_docker_state
    case $? in
        0)  _pcok   "$(_t 'Docker 可用且当前用户能调用 docker ps' 'Docker reachable; current user can run docker ps')" ;;
        10) _pcfail "$(_t 'Docker 未安装' 'Docker not installed')"
            print_fix "$(_t 'Debian/Ubuntu 安装：apt update && apt install -y docker.io' 'Debian/Ubuntu install: apt update && apt install -y docker.io')" ;;
        11) _pcfail "$(_t 'Docker daemon 未运行' 'Docker daemon not running')"
            print_fix "$(_t '启动 daemon：service docker start  或  systemctl start docker' 'Start daemon: service docker start  OR  systemctl start docker')" ;;
        12) _pcfail "$(_t '当前用户无 /var/run/docker.sock 权限' 'Current user lacks /var/run/docker.sock permission')"
            print_fix "$(_t '永久路：usermod -aG docker $USER  然后重新登录终端' 'Permanent: usermod -aG docker $USER  then re-login')"
            print_fix "$(_t '临时路：用 sudo 重跑此脚本，或切到 root' 'Quick: rerun with sudo, or switch to root')" ;;
    esac
    if docker compose version >/dev/null 2>&1; then
        _pcok "$(_t 'docker compose 可用 (v2 plugin)' 'docker compose available (v2 plugin)')"
    elif command -v docker-compose >/dev/null 2>&1; then
        _pcok "$(_t 'docker-compose 可用 (v1 binary)' 'docker-compose available (v1 binary)')"
    else
        _pcwarn "$(_t '未发现 docker compose（不影响一键脚本，docker run 直跑即可）' 'docker compose not found (script does not need it)')"
    fi
    if container_exists; then
        _pcok "$(_t "已有容器 $NAME（升级 / 重新部署可继续）" "Container $NAME already exists (update / redeploy can proceed)")"
    else
        _pcwarn "$(_t "尚无容器 $NAME（首次安装）" "No $NAME container yet (first install)")"
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        _pcok "$(_t "已有镜像 $DEFAULT_IMAGE" "Image $DEFAULT_IMAGE present")"
    else
        _pcwarn "$(_t "镜像 $DEFAULT_IMAGE 不存在（安装流程会引导构建或拉取）" "Image $DEFAULT_IMAGE not present (install flow will build or pull)")"
    fi
}

file_precheck() {
    print_step "$(_t '预检: 文件与目录' 'PreCheck: files and directories')"
    local d
    for d in "$CONFIG_DIR" "$CERTS_DIR" "$BACKUP_DIR"; do
        if [[ -d "$d" ]]; then _pcok "$(_t "目录存在：$d" "Directory exists: $d")"
        else _pcwarn "$(_t "目录不存在：$d（安装流程会自动创建）" "Directory missing: $d (install flow will create)")"
        fi
    done
    if [[ -f "$CONFIG_FILE" ]]; then
        if jq empty "$CONFIG_FILE" 2>/dev/null; then
            _pcok "$(_t 'config.json 存在且 JSON 合法' 'config.json present and valid JSON')"
        else
            _pcfail "$(_t "config.json 存在但非合法 JSON：$CONFIG_FILE" "config.json present but not valid JSON: $CONFIG_FILE")"
            print_fix "$(_t '用菜单 11 / yunzes-node edit-config 修复' 'Use menu 11 / yunzes-node edit-config to fix')"
        fi
    else
        _pcwarn "$(_t "config.json 不存在：$CONFIG_FILE（安装流程会引导生成）" "config.json missing: $CONFIG_FILE (install flow will create)")"
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
            [[ "$port" == "?" || -z "$port" || -z "$proto" ]] && continue
            _check_port_free "$port" "$proto" || true
        done < "$tmp"
        rm -f "$tmp"
    fi
    if command -v ufw >/dev/null 2>&1; then
        local ufw_state
        ufw_state="$(ufw status 2>/dev/null | head -1 || true)"
        _pcwarn "$(_t "检测到 ufw（$ufw_state）；请确保业务端口已放行" "ufw detected ($ufw_state); ensure business ports are allowed")"
    elif command -v firewall-cmd >/dev/null 2>&1; then
        _pcwarn "$(_t '检测到 firewalld；请确保业务端口已放行' 'firewalld detected; ensure business ports are allowed')"
    fi
}

precheck() {
    reset_precheck_counters
    basic_precheck      "${1:-}"
    dependency_precheck
    docker_precheck
    file_precheck
    echo
    print_info "$(_t '预检汇总:' 'PreCheck summary:')"
    printf "  %bPASS%b: %b%d%b\n" "${C_GREEN}"  "${C_PLAIN}" "${C_GREEN}"  "$PRECHECK_PASS" "${C_PLAIN}"
    printf "  %bWARN%b: %b%d%b\n" "${C_YELLOW}" "${C_PLAIN}" "${C_YELLOW}" "$PRECHECK_WARN" "${C_PLAIN}"
    printf "  %bFAIL%b: %b%d%b\n" "${C_RED}"    "${C_PLAIN}" "${C_RED}"    "$PRECHECK_FAIL" "${C_PLAIN}"
    if (( PRECHECK_FAIL > 0 )); then
        print_fail "$(_t '预检不通过，安装/升级流程中断。修完上面的 [FAIL] 项再重试。' 'PreCheck failed. Fix [FAIL] items above then retry.')"
        return 1
    fi
    return 0
}

_check_port_free() {
    local port="$1" proto="$2"
    [[ "$port" == "?" || -z "$port" ]] && return 0
    if ! command -v ss >/dev/null 2>&1; then return 0; fi
    local hits owner
    case "$proto" in
        tcp) hits=$(ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
        udp) hits=$(ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
        *)   return 0 ;;
    esac
    [[ -z "$hits" ]] && return 0
    owner=$(echo "$hits" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
    if [[ "$owner" == "$NAME" ]]; then
        _pcok "$(_t "$port/$proto 已由 yunzes-node 自身监听（属正常）" "$port/$proto already owned by yunzes-node (expected)")"
    else
        _pcwarn "$(_t "$port/$proto 已被 ${owner:-未知进程} 占用；ACME / 节点端口可能冲突" "$port/$proto held by ${owner:-unknown}; may conflict with ACME / node port")"
    fi
}

# ============================================================================
# M14 validate / M13 panel-probe: config helpers — parse listen specs from
# config.json, validate per-node fields, probe panel API reachability.
# ============================================================================
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

# validate_config returns 0 iff every Node passes the cert / id / type checks.
# This catches the "user pressed Enter on CertDomain" footgun BEFORE the
# container restart loop kicks in.
validate_config() {
    [[ -f "$CONFIG_FILE" ]] || { print_fail "$(_t "$CONFIG_FILE 不存在" "$CONFIG_FILE missing")"; return 1; }
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        print_fail "$(_t 'config.json 非合法 JSON' 'config.json not valid JSON')"
        return 1
    fi
    print_step "$(_t '配置体检' 'Config sanity check')"
    local errors=0 warns=0 total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE")

    # Smart mode detection: top-level .Api block populated AND .Nodes empty
    # implies cmd/server.go will route through nodes.StartNodes (panel-driven
    # multi-protocol). Per-node validation does not apply — the panel decides
    # NodeType / Cert / Port at runtime.
    local api_host api_server_id api_secret
    api_host=$(jq    -r '.Api.ApiHost   // ""' "$CONFIG_FILE")
    api_server_id=$(jq -r '.Api.ServerID  // 0'  "$CONFIG_FILE")
    api_secret=$(jq  -r '.Api.SecretKey // ""' "$CONFIG_FILE")
    if [[ -n "$api_host" && "$api_server_id" != "0" && -n "$api_secret" && "$total" == "0" ]]; then
        print_ok "$(_t "智能模式：面板=$api_host  ServerID=$api_server_id" \
                       "Smart mode: panel=$api_host  ServerID=$api_server_id")"
        print_ok "$(_t '配置体检通过（智能模式由面板下发协议参数，本地无需 NodeID/NodeType 校验）' \
                       'Config validation passed (smart mode — panel supplies per-protocol fields)')"
        return 0
    fi

    if (( total == 0 )); then
        print_fail "$(_t '配置中既无 .Nodes 也无 .Api 智能模式' 'No .Nodes entries and no .Api smart-mode block')"
        return 1
    fi
    local seen_ids=()
    local idx node_id node_type cert_mode cert_domain
    for ((idx=0; idx<total; idx++)); do
        node_id=$(jq -r   ".Nodes[$idx].NodeID  // empty" "$CONFIG_FILE")
        node_type=$(jq -r ".Nodes[$idx].NodeType // empty" "$CONFIG_FILE")
        cert_mode=$(jq -r ".Nodes[$idx].CertConfig.CertMode // .Nodes[$idx].Options.CertConfig.CertMode // \"none\"" "$CONFIG_FILE")
        cert_domain=$(jq -r ".Nodes[$idx].CertConfig.CertDomain // .Nodes[$idx].Options.CertConfig.CertDomain // \"\"" "$CONFIG_FILE")

        if [[ -z "$node_type" ]]; then
            print_fail "$(_t "节点 #$idx: NodeType 缺失" "Node #$idx: NodeType missing")"
            errors=$((errors+1))
        else
            case "$node_type" in
                vless|vmess|trojan|shadowsocks|hysteria|hysteria2|tuic|anytls) ;;
                *) print_fail "$(_t "节点 #$idx: 未知 NodeType '$node_type'" "Node #$idx: unknown NodeType '$node_type'")"; errors=$((errors+1)) ;;
            esac
        fi

        if [[ -z "$node_id" || "$node_id" == "null" ]]; then
            print_fail "$(_t "节点 #$idx: NodeID 缺失" "Node #$idx: NodeID missing")"
            errors=$((errors+1))
        else
            local seen
            for seen in "${seen_ids[@]:-}"; do
                if [[ "$seen" == "$node_id" ]]; then
                    print_fail "$(_t "节点 #$idx: NodeID=$node_id 重复（同一容器不能有两个相同 NodeID）" "Node #$idx: duplicate NodeID=$node_id (forbidden in one container)")"
                    errors=$((errors+1))
                fi
            done
            seen_ids+=("$node_id")
        fi

        case "$cert_mode" in
            none|"") ;;
            file|self|http|dns)
                if [[ -z "$cert_domain" || "$cert_domain" == "null" ]]; then
                    print_fail "$(_t "节点 #$idx ($node_type, CertMode=$cert_mode): CertDomain 不能为空 — yunzes-node 的 EnsureCertificate 会拒绝并触发容器重启循环" \
                                    "Node #$idx ($node_type, CertMode=$cert_mode): CertDomain must not be empty — EnsureCertificate will reject this and the container will restart-loop")"
                    print_fix "$(_t "用菜单 11 / yunzes-node edit-config 填上 CertDomain，或把 CertMode 改成 \"none\"" \
                                    "Use menu 11 / yunzes-node edit-config to set CertDomain, or change CertMode to \"none\"")"
                    errors=$((errors+1))
                fi
                ;;
            *)
                print_warn "$(_t "节点 #$idx: 未知 CertMode '$cert_mode'，建议改为 none/file/self/http/dns 之一" "Node #$idx: unknown CertMode '$cert_mode' (use none/file/self/http/dns)")"
                warns=$((warns+1))
                ;;
        esac
    done
    if (( errors == 0 )); then
        print_ok "$(_t "validate_config 通过 ($total 个节点, $warns warning)" "validate_config passed ($total nodes, $warns warning)")"
        return 0
    else
        print_fail "$(_t "validate_config 失败: $errors 个错误" "validate_config failed: $errors error(s)")"
        return 1
    fi
}

# preflight_panel_check probes each node's actual API URL (not just the
# ApiHost root). Catches "wrong panel" / "wrong path version" before
# launching the container and chasing red herring restart loops.
preflight_panel_check() {
    [[ -f "$CONFIG_FILE" ]] || return 0
    jq empty "$CONFIG_FILE" 2>/dev/null || return 0
    print_step "$(_t '预检: 面板 API 可达性' 'Preflight: panel API reachability')"

    # Smart mode: probe /v2/server/{id} once instead of per-node /v1.
    local s_host s_sid s_key s_nodes
    s_host=$(jq -r '.Api.ApiHost   // ""' "$CONFIG_FILE")
    s_sid=$(jq  -r '.Api.ServerID  // 0'  "$CONFIG_FILE")
    s_key=$(jq  -r '.Api.SecretKey // ""' "$CONFIG_FILE")
    s_nodes=$(jq '.Nodes | length // 0'    "$CONFIG_FILE")
    if [[ -n "$s_host" && "$s_sid" != "0" && -n "$s_key" && "$s_nodes" == "0" ]]; then
        local url_safe="${s_host%/}/v2/server/${s_sid}?secret_key=***"
        print_cmd "curl -s --connect-timeout 5 $url_safe"
        local code
        code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 \
                "${s_host%/}/v2/server/${s_sid}?secret_key=${s_key}" || echo 000)
        case "$code" in
            200) print_ok "$(_t "面板 /v2/server/$s_sid: HTTP 200" "panel /v2/server/$s_sid: HTTP 200")"; return 0 ;;
            401|403)
                print_fail "$(_t "面板 /v2/server/$s_sid: HTTP $code — SecretKey 错或权限不足" "panel /v2/server/$s_sid: HTTP $code — wrong SecretKey")"
                ;;
            404)
                print_fail "$(_t "面板 /v2/server/$s_sid: HTTP 404 — 面板不支持智能模式" "panel /v2/server/$s_sid: HTTP 404 — panel does not implement smart mode")"
                ;;
            5*)
                print_warn "$(_t "面板 /v2/server/$s_sid: HTTP $code — 服务端错误" "panel /v2/server/$s_sid: HTTP $code — server error")"
                ;;
            000|"")
                print_fail "$(_t "无法连接面板 $s_host" "Cannot reach panel $s_host")"
                ;;
            *)
                print_info "$(_t "面板 /v2/server/$s_sid: HTTP $code" "panel /v2/server/$s_sid: HTTP $code")"
                ;;
        esac
        if [[ "$code" != "200" ]]; then
            if ! confirm "$(_t '是否仍然继续启动 (Y/n)' 'Continue starting anyway (Y/n)')" y; then
                return 1
            fi
        fi
        return 0
    fi

    local total
    total=$(jq '.Nodes | length // 0' "$CONFIG_FILE")
    local idx host node_id ntype path code key
    local fatal=0
    for ((idx=0; idx<total; idx++)); do
        host=$(jq -r    ".Nodes[$idx].ApiHost  // empty" "$CONFIG_FILE")
        node_id=$(jq -r ".Nodes[$idx].NodeID  // empty" "$CONFIG_FILE")
        ntype=$(jq -r   ".Nodes[$idx].NodeType // empty" "$CONFIG_FILE")
        key=$(jq -r     ".Nodes[$idx].ApiKey  // empty" "$CONFIG_FILE")
        if [[ -z "$host" ]]; then
            print_warn "$(_t "节点 #$idx: ApiHost 为空，跳过预检" "Node #$idx: ApiHost empty, skipping")"
            continue
        fi
        path="${host%/}/v1/server/config?protocol=${ntype}&server_id=${node_id}&secret_key=${key}"
        print_cmd "curl -s --connect-timeout 5 ${host%/}/v1/server/config?protocol=${ntype}&server_id=${node_id}&secret_key=***"
        code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$path" || true)
        case "$code" in
            200)
                print_ok "$(_t "节点 #$idx ($ntype/$node_id): HTTP 200 — 面板返回节点配置" "Node #$idx ($ntype/$node_id): HTTP 200 — panel returns node config")"
                ;;
            301|302|303|307|308)
                print_warn "$(_t "节点 #$idx: HTTP $code 重定向 — 检查 ApiHost 是否带 https://" "Node #$idx: HTTP $code redirect — check ApiHost protocol")"
                ;;
            401|403)
                print_warn "$(_t "节点 #$idx: HTTP $code — ApiKey 错误或权限不足" "Node #$idx: HTTP $code — wrong ApiKey or insufficient privilege")"
                ;;
            404)
                print_fail "$(_t "节点 #$idx: HTTP 404 — 路径或 NodeID 不存在；请确认面板与 yunzes-node 的 API 兼容（/v1/server/config）" \
                                "Node #$idx: HTTP 404 — path or NodeID not found; ensure panel speaks the /v1/server/config API")"
                fatal=$((fatal+1))
                ;;
            5*)
                print_warn "$(_t "节点 #$idx: HTTP $code — 面板内部错误" "Node #$idx: HTTP $code — panel internal error")"
                ;;
            000|"")
                print_fail "$(_t "节点 #$idx: 无法连接 $host" "Node #$idx: cannot connect to $host")"
                fatal=$((fatal+1))
                ;;
            *)
                print_info "$(_t "节点 #$idx: HTTP $code (具体含义参考面板文档)" "Node #$idx: HTTP $code (see your panel docs)")"
                ;;
        esac
    done
    if (( fatal > 0 )); then
        print_warn "$(_t "面板预检有 $fatal 项失败 — 容器启动后大概率出现 \"Run nodes failed\" 并触发重启循环" \
                        "panel preflight has $fatal FAIL(s) — container will likely log 'Run nodes failed' and restart-loop")"
        if ! confirm "$(_t '是否仍然继续启动 (Y/n)' 'Continue starting anyway (Y/n)')" y; then
            return 1
        fi
    fi
    return 0
}

# ============================================================================
# M09 backup: snapshot config.json + certs/ + docker inspect into
# /opt/yunzes-node/backups/<ts>/. List + restore primitives below.
# ============================================================================
backup_now() {
    mkdir -p "$BACKUP_DIR"
    local ts dir
    ts=$(date +%Y%m%d-%H%M%S)
    dir="$BACKUP_DIR/$ts"
    mkdir -p "$dir"
    [[ -f "$CONFIG_FILE" ]] && cp -p "$CONFIG_FILE" "$dir/config.json"
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
    [[ -d "$dir" ]] || { print_fail "$(_t "备份目录不存在：$dir" "Backup dir missing: $dir")"; return 1; }
    print_step "$(_t "回滚到备份：$name" "Restoring backup: $name")"
    if [[ -f "$dir/config.json" ]]; then
        cp -p "$dir/config.json" "$CONFIG_FILE"
        print_ok "$(_t 'config.json 恢复' 'config.json restored')"
    fi
    if [[ -f "$dir/certs.tar.gz" ]]; then
        tar -C "$CONFIG_DIR" -xzf "$dir/certs.tar.gz" 2>/dev/null \
            || print_warn "$(_t '证书包解压失败（可手动 tar -xzvf 查看）' 'certs.tar.gz extract failed')"
        print_ok "$(_t 'certs/ 恢复' 'certs/ restored')"
    fi
    local image_to_use=""
    [[ -f "$dir/image-name.txt" ]] && image_to_use="$(<"$dir/image-name.txt")"
    [[ -z "$image_to_use" ]] && image_to_use="$DEFAULT_IMAGE"
    print_info "$(_t "使用镜像：$image_to_use" "Using image: $image_to_use")"
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if ! _docker_run "$image_to_use" "always"; then
        print_fail "$(_t '回滚启动容器失败' 'Rollback container start failed')"
        return 1
    fi
    return 0
}

# ============================================================================
# M10 source: clone or update REPO_URL into SRC_DIR for local docker build.
# ============================================================================
fetch_or_use_source() {
    if [[ -d "$SRC_DIR/.git" ]]; then
        print_info "$(_t "已存在源码：$SRC_DIR" "Existing source: $SRC_DIR")" >&2
        print_choice "1)" "$(_t 'git pull (拉取最新)' 'git pull (latest)')"           >&2
        print_choice "2)" "$(_t '使用现有源码' 'Use existing source')"                  >&2
        print_choice "3)" "$(_t '删除后重新 clone' 'Wipe and re-clone')"                >&2
        print_choice "4)" "$(_t '退出' 'Exit')"                                         >&2
        local ans
        printf "%b%s%b" "${C_CYAN}" "$(_t '请选择 [1-4]: ' 'Select [1-4]: ')" "${C_PLAIN}" >&2
        read -r ans
        case "$ans" in
            1)
                print_cmd "git -C $SRC_DIR pull --ff-only" >&2
                if ! ( cd "$SRC_DIR" && git pull --ff-only ) >&2; then
                    print_warn "$(_t 'git pull 失败（可能本地有未提交改动）' 'git pull failed (possibly local changes)')" >&2
                    if ! confirm "$(_t "继续使用当前 $SRC_DIR 源码" "Continue with existing $SRC_DIR")" y; then
                        return 1
                    fi
                fi ;;
            2) print_info "$(_t "使用现有 $SRC_DIR" "Using existing $SRC_DIR")" >&2 ;;
            3)
                print_danger "$(_t "将删除 $SRC_DIR 重新 clone" "Will wipe $SRC_DIR and re-clone")" >&2
                if ! confirm "$(_t '继续' 'Continue')" n; then return 1; fi
                print_cmd "rm -rf $SRC_DIR" >&2
                rm -rf "$SRC_DIR"
                print_cmd "git clone $REPO_URL $SRC_DIR" >&2
                git clone "$REPO_URL" "$SRC_DIR" >&2 || return 1 ;;
            4) return 1 ;;
            *) print_warn "$(_t '无效选项，使用现有源码' 'Invalid option, using existing source')" >&2 ;;
        esac
    else
        mkdir -p "$(dirname "$SRC_DIR")"
        print_cmd "git clone $REPO_URL $SRC_DIR" >&2
        git clone "$REPO_URL" "$SRC_DIR" >&2 || return 1
    fi
    echo "$SRC_DIR"
}

# ============================================================================
# M11 container: canonical docker run wrapper + ensure_dirs.
# ============================================================================
_docker_run() {
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

# ============================================================================
# M12 verify: 3-tier reporter.
#   L1 verify_basic       container running + config valid + no panic
#   L2 verify_network     host network, real port-listen check (auto-diag on
#                         miss: who-owns-port + matching xray/sing warns)
#   L3 verify_business    panel reachability + structured-log marker presence
# ============================================================================
verify_basic() {
    local pass=0 myfail=0
    print_step "$(_t '验证 L1: 基础' 'Verify L1: basics')"
    if container_running; then
        print_ok "$(_t "容器 $NAME 运行中" "Container $NAME running")"; pass=$((pass+1))
        # Restart-loop detector: if a container has restarted >=3 times in
        # the last 60s it almost certainly hit an issue Go-side. We surface
        # this so verify_basic doesn't return PASS for a thrashing container.
        local restart_count finished_at
        restart_count=$(docker inspect --format '{{.RestartCount}}' "$NAME" 2>/dev/null || echo 0)
        finished_at=$( docker inspect --format '{{.State.FinishedAt}}' "$NAME" 2>/dev/null || true)
        if (( restart_count >= 3 )); then
            local recent_starts
            recent_starts=$(docker events --since 60s --until 0s --filter "container=$NAME" --filter event=start 2>/dev/null | wc -l || echo 0)
            if (( recent_starts >= 3 )); then
                print_fail "$(_t "RestartCount=$restart_count 且最近 60 秒发生 $recent_starts 次 start — 容器在重启循环" \
                                "RestartCount=$restart_count and $recent_starts starts in the last 60s — container is restart-looping")"
                print_fix "$(_t '查看 docker logs yunzes-node；或 yunzes-node show-config 检查 CertDomain / 配置' \
                                'Inspect docker logs yunzes-node; or yunzes-node show-config to verify CertDomain / config')"
                myfail=$((myfail+1))
            else
                print_warn "$(_t "RestartCount=$restart_count（历史累计；最近 60 秒看起来稳定）" \
                                "RestartCount=$restart_count (historic; recent 60s seems stable)")"
            fi
        fi
    else
        local last_status
        last_status=$(docker inspect --format '{{.State.Status}}' "$NAME" 2>/dev/null || true)
        case "$last_status" in
            restarting) print_fail "$(_t "容器 $NAME 状态: restarting (重启循环)" "Container $NAME status: restarting (loop)")" ;;
            exited)     print_fail "$(_t "容器 $NAME 状态: exited" "Container $NAME status: exited")" ;;
            *)          print_fail "$(_t "容器 $NAME 未运行" "Container $NAME not running")" ;;
        esac
        myfail=$((myfail+1))
    fi
    if [[ -f "$CONFIG_FILE" ]]; then
        if jq empty "$CONFIG_FILE" 2>/dev/null; then
            print_ok "$(_t 'config.json 存在且合法' 'config.json present and valid')"; pass=$((pass+1))
        else
            print_fail "$(_t 'config.json 存在但非合法 JSON' 'config.json present but invalid JSON')"; myfail=$((myfail+1))
        fi
    else
        print_fail "$(_t 'config.json 不存在' 'config.json missing')"; myfail=$((myfail+1))
    fi
    if [[ -d "$CERTS_DIR" ]]; then
        print_ok "$(_t 'certs/ 存在' 'certs/ present')"; pass=$((pass+1))
    else
        print_warn "$(_t 'certs/ 不存在（仅 cleartext / reality 节点可接受）' 'certs/ missing (acceptable for cleartext / reality only)')"
    fi
    local bad
    bad=$(docker logs "$NAME" 2>&1 | grep -E -i 'panic|runtime error|nil pointer dereference|segmentation violation|fatal error' | head -5 || true)
    if [[ -z "$bad" ]]; then
        print_ok "$(_t 'docker logs 无 panic / fatal / runtime error' 'docker logs has no panic / fatal / runtime error')"; pass=$((pass+1))
    else
        print_fail "$(_t 'docker logs 出现 panic / fatal / runtime error:' 'docker logs contains panic / fatal / runtime error:')"
        print_info "$(_t '以下为匹配到的原始日志行:' 'Matching raw log lines:')"
        echo "$bad" | sed 's/^/      /'
        myfail=$((myfail+1))
    fi
    printf "L1: %bPASS=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

extract_listens_from_logs() {
    docker logs --tail 1000 "$NAME" 2>&1 \
        | grep -F 'msg="Adding node inbound"' \
        | while IFS= read -r line; do
            local port proto network
            port=$(echo    "$line" | grep -oE 'port=[0-9]+'                | head -1 | cut -d= -f2)
            proto=$(echo   "$line" | grep -oE 'protocol=[a-z0-9]+'         | head -1 | cut -d= -f2)
            network=$(echo "$line" | grep -oE 'network="\[[^]]+\]"'        | head -1 | sed -E 's/network="\[([^]]+)\]"/\1/')
            [[ -z "$port" || -z "$network" ]] && continue
            local t
            for t in $network; do echo "$proto $port $t"; done
        done
}

_listen_active() {
    local port="$1" proto="$2"
    case "$proto" in
        tcp) ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' | grep -q .;;
        udp) ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' | grep -q .;;
        *)   return 1;;
    esac
}

verify_network() {
    local pass=0 myfail=0 mywarn=0
    print_step "$(_t '验证 L2: 网络' 'Verify L2: network')"
    local mode
    mode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$NAME" 2>/dev/null || true)
    if [[ "$mode" == "host" ]]; then
        print_ok "$(_t '容器使用 host network' 'Container in host network mode')"; pass=$((pass+1))
    elif [[ -z "$mode" ]]; then
        print_warn "$(_t '无法读取容器网络模式（容器可能不存在）' 'Cannot read network mode (container missing?)')"; mywarn=$((mywarn+1))
    else
        print_warn "$(_t "容器使用 $mode 而非 host network" "Container in $mode (production wants host)")"; mywarn=$((mywarn+1))
    fi
    if [[ ! -f "$CONFIG_FILE" ]] || ! command -v ss >/dev/null 2>&1; then
        printf "L2: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
            "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
        return $myfail
    fi
    local rows row p port t
    rows="$(extract_listens_from_logs)"
    if [[ -z "$rows" ]]; then
        print_warn "$(_t "docker logs 未找到 'Adding node inbound' 行（容器尚未上线节点？）" "docker logs has no 'Adding node inbound' line (no node started?)")"
        mywarn=$((mywarn+1))
    else
        while IFS= read -r row; do
            [[ -z "$row" ]] && continue
            read -r p port t <<<"$row"
            if _listen_active "$port" "$t"; then
                print_ok "$(_t "$p $port/$t 实际监听 ✓" "$p $port/$t actively listening ✓")"; pass=$((pass+1))
            else
                print_fail "$(_t "$p $port/$t 期待监听但未发现" "$p $port/$t expected listen NOT FOUND")"; myfail=$((myfail+1))
                # Auto-diagnostic: explain WHY the bind likely failed.
                # All locals initialized to empty so `set -u` doesn't trip if
                # the case below doesn't match (e.g. unknown transport) or
                # the pipeline produces no output.
                local owner_line="" owner_proc="" xray_warn="" reality_lines=""
                # 1. Is the port held by another process on the host?
                case "$t" in
                    tcp) owner_line=$(ss -lntp 2>/dev/null | awk -v pat=":$port" '$4 ~ pat"$"' | head -1 || true) ;;
                    udp) owner_line=$(ss -lnup 2>/dev/null | awk -v pat=":$port" '$4 ~ pat"$"' | head -1 || true) ;;
                esac
                if [[ -n "$owner_line" ]]; then
                    owner_proc=$(echo "$owner_line" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)
                    if [[ -n "$owner_proc" && "$owner_proc" != "$NAME" ]]; then
                        print_fix "$(_t "  $port/$t 被 $owner_proc 占用 — 改另一个端口或停掉 $owner_proc" "  $port/$t held by $owner_proc — pick another port or stop $owner_proc")"
                    fi
                fi
                # 2. xray / sing 内部 bind 错误（warn 级别，不会进 L1 panic 检测）
                xray_warn=$(docker logs --tail 500 "$NAME" 2>&1 \
                    | grep -E "(\[Warning\]|\[Error\]|level=(warn|warning|error)).*($port|$p)" \
                    | tail -3 || true)
                if [[ -n "$xray_warn" ]]; then
                    print_info "$(_t "  以下为 docker logs 中与 $p/$port/$t 相关的 warn/error:" "  Related warn/error from docker logs for $p/$port/$t:")"
                    echo "$xray_warn" | sed 's/^/      /'
                fi
                # 3. 配置 hint：reality 缺字段是常见 silent fail
                if [[ "$p" == "vless" ]]; then
                    reality_lines=$(docker logs --tail 300 "$NAME" 2>&1 | grep -i "reality" | tail -3 || true)
                    if [[ -n "$reality_lines" ]]; then
                        print_info "$(_t "  reality 相关日志:" "  reality-related lines:")"
                        echo "$reality_lines" | sed 's/^/      /'
                    fi
                fi
            fi
        done <<<"$rows"
        local ss_tcp ss_udp
        ss_tcp=$(echo "$rows" | awk '$1=="shadowsocks" && $3=="tcp"' | wc -l)
        ss_udp=$(echo "$rows" | awk '$1=="shadowsocks" && $3=="udp"' | wc -l)
        if (( ss_tcp + ss_udp > 0 )); then
            if (( ss_tcp == ss_udp )); then
                print_ok "$(_t "shadowsocks 节点 tcp+udp 配对一致 (各 $ss_tcp 个)" "shadowsocks tcp+udp paired ($ss_tcp each)")"; pass=$((pass+1))
            else
                print_fail "$(_t "shadowsocks tcp/udp 不配对 (tcp=$ss_tcp udp=$ss_udp)" "shadowsocks tcp/udp mismatch (tcp=$ss_tcp udp=$ss_udp)")"; myfail=$((myfail+1))
            fi
        fi
        local udp_only
        udp_only=$(echo "$rows" | awk '$1 ~ /^(hysteria2?|tuic)$/ && $3=="udp"' | wc -l)
        if (( udp_only > 0 )); then
            print_ok "$(_t "hysteria2 / tuic 节点 udp 监听 (共 $udp_only 个)" "hysteria2 / tuic udp listening ($udp_only)")"; pass=$((pass+1))
        fi
    fi
    local hits owner80
    hits=$(ss -lntp 2>/dev/null | awk '$4 ~ /:80$/' || true)
    if [[ -n "$hits" ]]; then
        owner80=$(echo "$hits" | grep -oE 'users:\(\("[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
        if [[ "$owner80" == "$NAME" ]]; then
            print_ok "$(_t '80/tcp 由 yunzes-node 自身监听' '80/tcp owned by yunzes-node')"; pass=$((pass+1))
        else
            print_warn "$(_t "80/tcp 被 ${owner80:-未知} 占用（若用 ACME HTTP-01 会失败）" "80/tcp held by ${owner80:-unknown} (ACME HTTP-01 will fail)")"; mywarn=$((mywarn+1))
        fi
    fi
    printf "L2: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

verify_business() {
    local pass=0 myfail=0 mywarn=0
    print_step "$(_t '验证 L3: 业务' 'Verify L3: business')"
    if [[ -f "$CONFIG_FILE" ]] && jq empty "$CONFIG_FILE" 2>/dev/null; then
        local hosts h code
        mapfile -t hosts < <(jq -r '.Nodes[]?.ApiHost // empty' "$CONFIG_FILE" | sort -u)
        for h in "${hosts[@]}"; do
            [[ -z "$h" ]] && continue
            code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$h" || true)
            case "$code" in
                2*|3*) print_ok   "$(_t "面板可达：$h  HTTP $code" "panel reachable: $h  HTTP $code")"; pass=$((pass+1)) ;;
                401|403) print_warn "$(_t "面板 401/403：$h  鉴权问题" "panel 401/403: $h  auth issue")"; mywarn=$((mywarn+1)) ;;
                404) print_warn "$(_t "面板 404：$h  路径可能不对" "panel 404: $h  wrong path?")"; mywarn=$((mywarn+1)) ;;
                5*) print_fail "$(_t "面板 $code：$h  服务端错误" "panel $code: $h  server error")"; myfail=$((myfail+1)) ;;
                000|"") print_fail "$(_t "无法连接：$h" "Cannot reach: $h")"; myfail=$((myfail+1)) ;;
                *) print_info "$(_t "面板 $code：$h" "panel $code: $h")" ;;
            esac
        done
    else
        print_warn "$(_t '无可用 config.json，跳过面板探活' 'No config.json, skipping panel probe')"; mywarn=$((mywarn+1))
    fi
    local logs
    logs="$(docker logs --tail 500 "$NAME" 2>&1 || true)"
    local marker
    for marker in "Start yunzes-node" "Core Selector" "Adding node inbound" "logical_tag" "core=" "runtime_key" "protocol=" "server_id" "port="; do
        if echo "$logs" | grep -qF "$marker"; then
            print_ok "$(_t "日志含字段：$marker" "log contains: $marker")"; pass=$((pass+1))
        else
            print_warn "$(_t "日志未见：$marker" "log missing: $marker")"; mywarn=$((mywarn+1))
        fi
    done
    printf "L3: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

# verify_certs_and_api is L4 — covers the gaps L1-L3 don't:
#
#   - L1 only checks that certs/ exists; it does NOT parse certs or
#     check expiry. Operators have shipped expired certs into restart
#     loops more than once because L1 was green.
#
#   - L3 only probes the ApiHost root URL; it does NOT exercise the
#     real /v1/server/config or /v2/server/<id> endpoints with the
#     actual secret_key, so a misconfigured key, wrong NodeID, or
#     panel build that doesn't implement the v2 path passes L3.
#
# This level walks each Node[]/Server entry, parses the on-disk cert
# (via openssl) when CertMode is non-"none", and probes the matching
# API path with the operator's real credentials.
verify_certs_and_api() {
    local pass=0 myfail=0 mywarn=0
    print_step "$(_t '验证 L4: 证书 + API 深度' 'Verify L4: certs + API depth')"

    if [[ ! -f "$CONFIG_FILE" ]] || ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        print_warn "$(_t '无可用 config.json，跳过 L4' 'No config.json, skipping L4')"
        printf "L4: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
            "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
        return 0
    fi
    if ! command -v openssl >/dev/null 2>&1; then
        print_warn "$(_t '未安装 openssl，证书检查跳过' 'openssl not installed, cert checks skipped')"
        mywarn=$((mywarn+1))
    fi
    if ! command -v curl >/dev/null 2>&1; then
        print_warn "$(_t '未安装 curl，API 深度检查跳过' 'curl not installed, API depth checks skipped')"
        mywarn=$((mywarn+1))
    fi

    # ---- per-node cert + API probes (legacy Nodes[] shape) ----
    local nodes_count
    nodes_count=$(jq '.Nodes | length // 0' "$CONFIG_FILE")
    if (( nodes_count > 0 )); then
        local i=0
        while (( i < nodes_count )); do
            local cm cf domain api_host api_key node_id node_type
            cm=$(jq -r       ".Nodes[$i].CertConfig.CertMode // .Nodes[$i].Options.CertConfig.CertMode // \"none\"" "$CONFIG_FILE")
            cf=$(jq -r       ".Nodes[$i].CertConfig.CertFile // .Nodes[$i].Options.CertConfig.CertFile // \"\"" "$CONFIG_FILE")
            domain=$(jq -r   ".Nodes[$i].CertConfig.CertDomain // .Nodes[$i].Options.CertConfig.CertDomain // \"\"" "$CONFIG_FILE")
            api_host=$(jq -r ".Nodes[$i].ApiHost // \"\"" "$CONFIG_FILE")
            api_key=$(jq -r  ".Nodes[$i].ApiKey  // \"\"" "$CONFIG_FILE")
            node_id=$(jq -r  ".Nodes[$i].NodeID  // \"\"" "$CONFIG_FILE")
            node_type=$(jq -r ".Nodes[$i].NodeType // \"\"" "$CONFIG_FILE")

            # Cert check
            case "$cm" in
                file|http|dns|self)
                    if command -v openssl >/dev/null 2>&1; then
                        if [[ -z "$cf" ]]; then
                            print_warn "$(_t "节点 #$i ($node_type, CertMode=$cm): CertFile 未配置，跳过证书解析" \
                                            "Node #$i ($node_type, CertMode=$cm): CertFile not set, skipping cert parse")"
                            mywarn=$((mywarn+1))
                        elif [[ ! -f "$cf" ]]; then
                            # http/dns modes may not have issued the cert yet on first start
                            if [[ "$cm" == "http" || "$cm" == "dns" ]]; then
                                print_warn "$(_t "节点 #$i: 证书文件 $cf 不存在（ACME $cm 首次签发可能未完成）" \
                                                "Node #$i: cert file $cf missing (first ACME $cm issue may be pending)")"
                                mywarn=$((mywarn+1))
                            else
                                print_fail "$(_t "节点 #$i ($cm 模式): 证书文件 $cf 不存在" \
                                                "Node #$i ($cm mode): cert file $cf missing")"
                                myfail=$((myfail+1))
                            fi
                        else
                            local notafter days_left
                            notafter=$(openssl x509 -in "$cf" -noout -enddate 2>/dev/null | cut -d= -f2 || true)
                            if [[ -z "$notafter" ]]; then
                                print_fail "$(_t "节点 #$i: 证书 $cf 解析失败（非 PEM 或损坏）" \
                                                "Node #$i: cert $cf unparseable (not PEM or corrupt)")"
                                myfail=$((myfail+1))
                            elif openssl x509 -in "$cf" -noout -checkend 0 >/dev/null 2>&1; then
                                # not expired now; check 30-day window
                                local expire_ts now_ts
                                expire_ts=$(date -d "$notafter" +%s 2>/dev/null || echo 0)
                                now_ts=$(date +%s)
                                if (( expire_ts > 0 )); then
                                    days_left=$(( (expire_ts - now_ts) / 86400 ))
                                    if (( days_left < 7 )); then
                                        print_fail "$(_t "节点 #$i: 证书 $cf 仅剩 $days_left 天到期 ($notafter)" \
                                                        "Node #$i: cert $cf expires in $days_left days ($notafter)")"
                                        myfail=$((myfail+1))
                                    elif (( days_left < 30 )); then
                                        print_warn "$(_t "节点 #$i: 证书 $cf 剩 $days_left 天到期 ($notafter)" \
                                                        "Node #$i: cert $cf expires in $days_left days ($notafter)")"
                                        mywarn=$((mywarn+1))
                                    else
                                        print_ok "$(_t "节点 #$i: 证书 $cf 剩 $days_left 天 ($domain)" \
                                                        "Node #$i: cert $cf $days_left days remaining ($domain)")"
                                        pass=$((pass+1))
                                    fi
                                fi
                            else
                                print_fail "$(_t "节点 #$i: 证书 $cf 已过期 ($notafter)" \
                                                "Node #$i: cert $cf EXPIRED ($notafter)")"
                                myfail=$((myfail+1))
                            fi
                        fi
                    fi
                    ;;
                none|"")
                    : # no cert expected
                    ;;
                *)
                    print_warn "$(_t "节点 #$i: 未知 CertMode '$cm'，跳过证书检查" \
                                    "Node #$i: unknown CertMode '$cm', skipping cert check")"
                    mywarn=$((mywarn+1))
                    ;;
            esac

            # API depth probe (/v1/server/config) — exercises secret_key auth.
            if command -v curl >/dev/null 2>&1 && [[ -n "$api_host" && -n "$api_key" && -n "$node_id" ]]; then
                local url body status
                url="${api_host%/}/v1/server/config?protocol=${node_type}&server_id=${node_id}&secret_key=${api_key}"
                status=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 8 "$url" || echo 000)
                case "$status" in
                    200|304)
                        print_ok "$(_t "节点 #$i API: /v1/server/config 鉴权通过 (HTTP $status)" \
                                        "Node #$i API: /v1/server/config auth OK (HTTP $status)")"
                        pass=$((pass+1)) ;;
                    401|403)
                        print_fail "$(_t "节点 #$i API: secret_key 鉴权失败 (HTTP $status) — 检查 ApiKey" \
                                        "Node #$i API: secret_key auth failed (HTTP $status) — check ApiKey")"
                        myfail=$((myfail+1)) ;;
                    404)
                        print_fail "$(_t "节点 #$i API: NodeID=$node_id / NodeType=$node_type 在面板上不存在 (HTTP 404)" \
                                        "Node #$i API: NodeID=$node_id / NodeType=$node_type not found on panel (HTTP 404)")"
                        myfail=$((myfail+1)) ;;
                    5*)
                        print_fail "$(_t "节点 #$i API: 面板 5xx ($status)" \
                                        "Node #$i API: panel 5xx ($status)")"
                        myfail=$((myfail+1)) ;;
                    000)
                        print_fail "$(_t "节点 #$i API: 无法连接 $api_host" \
                                        "Node #$i API: cannot reach $api_host")"
                        myfail=$((myfail+1)) ;;
                    *)
                        print_warn "$(_t "节点 #$i API: HTTP $status (非典型)" \
                                        "Node #$i API: HTTP $status (atypical)")"
                        mywarn=$((mywarn+1)) ;;
                esac
            fi
            i=$((i+1))
        done
    fi

    # ---- panel-mode (ServerApiConfig + /v2/server/<id>) probe ----
    local sapi_host sapi_key sapi_sid
    sapi_host=$(jq -r '.ServerApiConfig.ApiHost   // empty' "$CONFIG_FILE")
    sapi_key=$( jq -r '.ServerApiConfig.SecretKey // empty' "$CONFIG_FILE")
    sapi_sid=$( jq -r '.ServerApiConfig.ServerId  // empty' "$CONFIG_FILE")
    if command -v curl >/dev/null 2>&1 && [[ -n "$sapi_host" && -n "$sapi_key" && -n "$sapi_sid" ]]; then
        local url status
        url="${sapi_host%/}/v2/server/${sapi_sid}?secret_key=${sapi_key}"
        status=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 8 "$url" || echo 000)
        case "$status" in
            200)
                print_ok "$(_t "面板 v2 API: /v2/server/$sapi_sid 鉴权通过 (HTTP 200)" \
                                "Panel v2 API: /v2/server/$sapi_sid auth OK (HTTP 200)")"
                pass=$((pass+1)) ;;
            401|403)
                print_fail "$(_t "面板 v2 API: secret_key 鉴权失败 (HTTP $status)" \
                                "Panel v2 API: secret_key auth failed (HTTP $status)")"
                myfail=$((myfail+1)) ;;
            404)
                print_warn "$(_t "面板 v2 API: 路径或 ServerID 不存在 (HTTP 404) — 节点会回落到 /v1 探测模式" \
                                "Panel v2 API: path or ServerID missing (HTTP 404) — node falls back to /v1 probe")"
                mywarn=$((mywarn+1)) ;;
            5*)
                print_fail "$(_t "面板 v2 API: 5xx ($status)" \
                                "Panel v2 API: 5xx ($status)")"
                myfail=$((myfail+1)) ;;
            000)
                print_fail "$(_t "面板 v2 API: 无法连接 $sapi_host" \
                                "Panel v2 API: cannot reach $sapi_host")"
                myfail=$((myfail+1)) ;;
            *)
                print_info "$(_t "面板 v2 API: HTTP $status" "Panel v2 API: HTTP $status")" ;;
        esac
    fi

    printf "L4: %bPASS=%d%b  %bWARN=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$pass" "${C_PLAIN}" "${C_YELLOW}" "$mywarn" "${C_PLAIN}" "${C_RED}" "$myfail" "${C_PLAIN}"
    return $myfail
}

cmd_verify() {
    verify_basic;          local r1=$?; echo
    verify_network;        local r2=$?; echo
    verify_business;       local r3=$?; echo
    verify_certs_and_api;  local r4=$?; echo
    print_separator
    if (( r1 + r2 + r3 + r4 == 0 )); then
        print_ok "$(_t '全部 verify 等级通过' 'All verify levels passed')"
        return 0
    fi
    print_fail "$(_t 'verify 有 FAIL 项；查看上方输出' 'verify has FAIL items; see output above')"
    return 1
}

# ============================================================================
# M16/M17/M18/M19 lifecycle: install/update/redeploy + start/stop/restart/
# status + logs + edit/show/gen-config. cmd_install ties everything together
# (precheck -> deps -> docker_precheck -> dirs -> config -> validate -> panel
# preflight -> docker run -> verify).
# ============================================================================
cmd_install() {
    parse_flags "$@"
    reset_precheck_counters
    basic_precheck install
    if (( PRECHECK_FAIL > 0 )); then
        print_fail "$(_t 'basic_precheck 不通过 — 修完上面的 [FAIL] 项再重试' 'basic_precheck failed — fix [FAIL] items above')"
        return 1
    fi
    if ! ensure_dependencies; then return 1; fi
    reset_precheck_counters
    docker_precheck
    (( PRECHECK_FAIL > 0 )) && { print_fail "$(_t 'docker_precheck 不通过 — 检查 Docker daemon / 用户组' 'docker_precheck failed — check Docker daemon / user group')"; return 1; }

    ensure_dirs

    if [[ -f "$CONFIG_FILE" ]]; then
        print_info "$(_t "已检测到 $CONFIG_FILE" "Detected $CONFIG_FILE")"
        print_choice "1)" "$(_t '使用现有配置'    'Use existing config')"
        print_choice "2)" "$(_t '备份后重新生成'  'Back up and regenerate')"
        print_choice "3)" "$(_t '退出'            'Exit')"
        local ans
        ans=$(prompt_read "$(_t '请选择' 'Select')" "1")
        case "$ans" in
            1) print_info "$(_t '使用现有配置' 'Using existing config')" ;;
            2)
                local b
                b=$(backup_now)
                print_ok "$(_t "已备份到 $b" "Backed up to $b")"
                cmd_gen_config ;;
            3) print_info "$(_t '用户取消' 'User cancelled')"; return 0 ;;
            *) print_warn "$(_t '无效选项，使用现有配置' 'Invalid option, using existing config')" ;;
        esac
    else
        print_info "$(_t 'config.json 不存在，进入配置生成流程' 'config.json missing, entering config generation')"
        cmd_gen_config
        if [[ ! -f "$CONFIG_FILE" ]]; then
            print_warn "$(_t '未生成 config.json — 请重跑 install 或手动写入' 'config.json not generated — re-run install or write manually')"
            return 0
        fi
    fi

    file_precheck

    # NEW: validate the config we just generated / kept BEFORE building
    # the image. Catches empty CertDomain, dup NodeID etc. fast.
    if ! validate_config; then
        print_fail "$(_t '配置体检失败；请用 yunzes-node edit-config 修复后重试' 'Config validation failed; fix via yunzes-node edit-config and retry')"
        return 1
    fi

    if ! docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        print_info "$(_t "镜像 $DEFAULT_IMAGE 不存在。请选择镜像来源:" "Image $DEFAULT_IMAGE missing. Choose source:")"
        print_choice "1)" "$(_t '从 GitHub 拉源码并本地构建 (推荐)' 'Clone from GitHub and build locally (recommended)')"
        print_choice "2)" "$(_t '使用当前目录源码构建'              'Build from current source checkout')"
        print_choice "3)" "$(_t '拉取远程 Docker 镜像'              'Pull remote Docker image')"
        print_choice "4)" "$(_t '手动输入镜像名'                    'Specify image name manually')"
        local choice img d
        choice=$(prompt_read "$(_t '请选择' 'Select')" "1")
        case "$choice" in
            1)
                d=$(fetch_or_use_source) || { print_fail "$(_t '源码获取失败' 'Source fetch failed')"; return 1; }
                print_step "$(_t "在 $d 执行 docker build" "docker build in $d")"
                print_cmd "docker build -t $DEFAULT_IMAGE ."
                if ! ( cd "$d" && docker build -t "$DEFAULT_IMAGE" . ); then
                    print_fail "$(_t 'docker build 失败' 'docker build failed')"; return 1
                fi
                print_ok "$(_t "镜像构建完成: $DEFAULT_IMAGE" "Image built: $DEFAULT_IMAGE")" ;;
            2)
                if [[ -z "$SOURCE_DIR" ]]; then
                    print_fail "$(_t '未在源码目录运行；请改用选项 1 / 3 / 4' 'Not in source checkout; use option 1 / 3 / 4 instead')"
                    return 1
                fi
                print_cmd "docker build -t $DEFAULT_IMAGE ."
                ( cd "$SOURCE_DIR" && docker build -t "$DEFAULT_IMAGE" . ) \
                    || { print_fail "$(_t 'docker build 失败' 'docker build failed')"; return 1; } ;;
            3)
                img=$(prompt_read "$(_t '拉取的镜像名' 'Image to pull')" "$DEFAULT_IMAGE")
                print_cmd "docker pull $img"
                docker pull "$img" || { print_fail "$(_t 'docker pull 失败' 'docker pull failed')"; return 1; }
                if [[ "$img" != "$DEFAULT_IMAGE" ]]; then
                    print_cmd "docker tag $img $DEFAULT_IMAGE"
                    docker tag "$img" "$DEFAULT_IMAGE"
                fi
                print_ok "$(_t "已 tag 为 $DEFAULT_IMAGE" "Tagged as $DEFAULT_IMAGE")" ;;
            4)
                img=$(prompt_read "$(_t '镜像名' 'Image name')" "")
                [[ -z "$img" ]] && { print_fail "$(_t '镜像名为空' 'Image name empty')"; return 1; }
                print_cmd "docker tag $img $DEFAULT_IMAGE"
                docker tag "$img" "$DEFAULT_IMAGE" \
                    || { print_fail "$(_t 'tag 失败' 'tag failed')"; return 1; } ;;
            *) print_fail "$(_t '无效选项' 'Invalid option')"; return 1 ;;
        esac
    else
        print_info "$(_t "已存在镜像 $DEFAULT_IMAGE — 复用" "Reusing existing image $DEFAULT_IMAGE")"
    fi

    # NEW: panel-API preflight after we have an image but BEFORE running.
    # Catches "wrong panel" before the container restart-loops.
    preflight_panel_check || return 1

    local b
    b=$(backup_now)
    print_info "$(_t "安装前备份: $b" "Pre-install backup: $b")"

    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    print_step "$(_t "启动容器 (restart=$RESTART_POLICY)" "Starting container (restart=$RESTART_POLICY)")"
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        print_fail "$(_t 'docker run 失败' 'docker run failed')"
        return 1
    fi
    print_ok "$(_t "容器已启动: $NAME" "Container started: $NAME")"

    sleep 4
    cmd_verify || print_warn "$(_t 'verify 有未通过项，请查看上方输出' 'verify has issues — see output above')"
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
        print_warn "$(_t '容器不存在；切换到 install 流程' 'Container missing; switching to install')"
        cmd_install "$@"
        return $?
    fi

    if ! validate_config; then
        if ! confirm "$(_t '当前 config 有 FAIL 项，是否仍然继续升级' 'Current config has FAIL; continue upgrade anyway')" n; then
            return 1
        fi
    fi

    local backup_dir previous_image
    backup_dir=$(backup_now)
    print_ok "$(_t "升级前已备份: $backup_dir" "Pre-upgrade backup: $backup_dir")"
    previous_image=$(current_image_id)

    print_info "$(_t '升级镜像来源:' 'Upgrade image source:')"
    print_choice "1)" "$(_t '从 GitHub 拉源码并本地构建 (与最新 commit 对齐)' 'Clone from GitHub and build (aligned with latest commit)')"
    print_choice "2)" "$(_t '使用当前目录源码构建' 'Build from current source checkout')"
    print_choice "3)" "$(_t '拉取远程镜像' 'Pull remote image')"
    print_choice "4)" "$(_t '跳过镜像更新（仅重新创建容器）' 'Skip image update (only recreate container)')"
    local choice img d
    choice=$(prompt_read "$(_t '请选择' 'Select')" "1")
    case "$choice" in
        1) d=$(fetch_or_use_source) || return 1
           print_cmd "docker build -t $DEFAULT_IMAGE ."
           ( cd "$d" && docker build -t "$DEFAULT_IMAGE" . ) || { print_fail "build failed"; return 1; } ;;
        2) [[ -z "$SOURCE_DIR" ]] && { print_fail "$(_t '未在源码目录运行' 'Not in source checkout')"; return 1; }
           print_cmd "docker build -t $DEFAULT_IMAGE ."
           ( cd "$SOURCE_DIR" && docker build -t "$DEFAULT_IMAGE" . ) || { print_fail "build failed"; return 1; } ;;
        3) img=$(prompt_read "$(_t '镜像名' 'Image name')" "$DEFAULT_IMAGE")
           print_cmd "docker pull $img"
           docker pull "$img" || { print_fail "$(_t 'pull 失败' 'pull failed')"; return 1; }
           [[ "$img" != "$DEFAULT_IMAGE" ]] && { print_cmd "docker tag $img $DEFAULT_IMAGE"; docker tag "$img" "$DEFAULT_IMAGE"; } ;;
        4) print_info "$(_t '跳过镜像更新' 'Skipping image update')" ;;
        *) print_fail "$(_t '无效选项' 'Invalid option')"; return 1 ;;
    esac

    print_step "$(_t '停止旧容器' 'Stopping old container')"
    print_cmd "docker stop $NAME && docker rm $NAME"
    docker stop "$NAME" >/dev/null 2>&1 || true
    docker rm   "$NAME" >/dev/null 2>&1 || true

    print_step "$(_t "启动新容器 (restart=$RESTART_POLICY)" "Starting new container (restart=$RESTART_POLICY)")"
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        print_warn "$(_t '新容器启动失败，触发自动回滚' 'New container start failed, auto-rollback')"
        _auto_rollback "$backup_dir" "$previous_image"
        return 1
    fi
    sleep 4
    if ! verify_basic; then
        print_warn "$(_t 'verify L1 失败，触发自动回滚' 'verify L1 failed, auto-rollback')"
        _auto_rollback "$backup_dir" "$previous_image"
        return 1
    fi
    print_ok "$(_t '升级完成' 'Upgrade complete')"
    cmd_verify || print_warn "$(_t 'verify 有未通过项' 'verify has issues')"
}

_auto_rollback() {
    local backup_dir="$1" prev="$2"
    print_step "$(_t "Auto rollback → $backup_dir" "Auto rollback → $backup_dir")"
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    [[ -f "$backup_dir/config.json"  ]] && cp -p "$backup_dir/config.json" "$CONFIG_FILE"
    [[ -f "$backup_dir/certs.tar.gz" ]] && tar -C "$CONFIG_DIR" -xzf "$backup_dir/certs.tar.gz" 2>/dev/null || true
    local image_to_use="$prev"
    if [[ -z "$image_to_use" || "$image_to_use" == "<no value>" ]]; then
        [[ -f "$backup_dir/image-name.txt" ]] && image_to_use="$(<"$backup_dir/image-name.txt")"
    fi
    [[ -z "$image_to_use" ]] && image_to_use="$DEFAULT_IMAGE"
    print_info "$(_t "rollback image: $image_to_use" "rollback image: $image_to_use")"
    if _docker_run "$image_to_use" "always"; then
        sleep 3
        verify_basic && { print_ok "$(_t '回滚成功' 'Rollback succeeded')"; return 0; }
    fi
    print_fail "$(_t "回滚启动失败 — 容器状态请用 yunzes-node status / docker logs $NAME 查看" \
                    "Rollback failed — check yunzes-node status / docker logs $NAME")"
    return 1
}

cmd_start() {
    if container_exists; then
        if container_running; then
            print_info "$(_t "$NAME 已经在运行" "$NAME is already running")"
        else
            print_cmd "docker start $NAME"
            docker start "$NAME" >/dev/null && print_ok "$(_t '已启动' 'Started')" || print_fail "$(_t '启动失败' 'Start failed')"
        fi
    else
        print_warn "$(_t '容器不存在；执行 yunzes-node install 先安装' 'Container missing; run yunzes-node install first')"
    fi
}

cmd_stop() {
    if container_running; then
        print_cmd "docker stop $NAME"
        docker stop "$NAME" >/dev/null && print_ok "$(_t '已停止' 'Stopped')"
    else
        print_info "$(_t "$NAME 未在运行" "$NAME is not running")"
    fi
}

cmd_restart() {
    if container_exists; then
        print_cmd "docker restart $NAME"
        docker restart "$NAME" >/dev/null && print_ok "$(_t '已重启' 'Restarted')"
    else
        print_warn "$(_t '容器不存在' 'Container missing')"
    fi
}

cmd_redeploy() {
    parse_flags "$@"
    if ! container_exists; then
        print_warn "$(_t '容器不存在；切换到 install' 'Container missing; switching to install')"
        cmd_install "$@"
        return $?
    fi
    validate_config || return 1
    local backup_dir
    backup_dir=$(backup_now)
    print_ok "$(_t "重新部署前备份: $backup_dir" "Pre-redeploy backup: $backup_dir")"
    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if ! _docker_run "$DEFAULT_IMAGE" "$RESTART_POLICY"; then
        print_fail "$(_t '启动失败，自动回滚' 'Start failed, auto-rollback')"
        _auto_rollback "$backup_dir" "$DEFAULT_IMAGE"
        return 1
    fi
    sleep 4
    cmd_verify || print_warn "$(_t 'verify 有未通过项' 'verify has issues')"
}

cmd_status() {
    if ! container_exists; then
        print_info "$(_t "$NAME 容器不存在" "$NAME container missing")"
        return 0
    fi
    print_info "$(_t '以下为 docker ps 原始输出:' 'Raw docker ps output below:')"
    docker ps -a --filter "name=^${NAME}$" --format "table {{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Ports}}"
    echo
    print_info "$(_t '以下为 docker stats 原始输出:' 'Raw docker stats output below:')"
    docker stats --no-stream "$NAME" 2>/dev/null || true
}

cmd_logs() {
    local n="${1:-100}"
    case "$n" in
        100|300) ;;
        ''|*[!0-9]*) n=100 ;;
    esac
    print_info "$(_t "以下为 docker logs --tail $n 原始输出:" "Raw docker logs --tail $n output below:")"
    docker logs --tail "$n" "$NAME" 2>&1 || print_warn "$(_t '容器不存在' 'Container missing')"
}

cmd_follow_log() {
    print_info "$(_t '跟随 docker logs (Ctrl-C 退出):' 'Following docker logs (Ctrl-C to exit):')"
    docker logs -f "$NAME" 2>&1 || print_warn "$(_t '容器不存在' 'Container missing')"
}

cmd_edit_config() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        print_warn "$(_t "$CONFIG_FILE 不存在；先用 yunzes-node gen-config 生成" "$CONFIG_FILE missing; run yunzes-node gen-config first")"
        return 1
    fi
    local editor="${EDITOR:-}"
    if [[ -z "$editor" ]]; then
        if   command -v nano >/dev/null 2>&1; then editor=nano
        elif command -v vi   >/dev/null 2>&1; then editor=vi
        else print_fail "$(_t '未找到 nano/vi，请设置 EDITOR 环境变量' 'No nano/vi found; set the EDITOR env var')"; return 1
        fi
    fi
    local backup
    backup=$(backup_now)
    print_info "$(_t "已备份当前配置: $backup" "Backed up current config: $backup")"
    cp -p "$CONFIG_FILE" "${CONFIG_FILE}.bak"
    print_cmd "$editor $CONFIG_FILE"
    "$editor" "$CONFIG_FILE"
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        print_fail "$(_t '保存的 config.json 非合法 JSON; 恢复 .bak' 'Saved config.json invalid JSON; restoring .bak')"
        mv "${CONFIG_FILE}.bak" "$CONFIG_FILE"
        return 1
    fi
    rm -f "${CONFIG_FILE}.bak"
    print_ok "$(_t 'JSON 校验通过' 'JSON syntax OK')"
    if ! validate_config; then
        print_warn "$(_t 'validate_config 报错；建议继续修复后再 restart' 'validate_config has FAIL; fix before restart')"
        return 1
    fi
    if container_running && confirm "$(_t '立即重启容器使配置生效' 'Restart container now to apply')" y; then
        cmd_restart
    fi
}

cmd_show_config() {
    if [[ ! -f "$CONFIG_FILE" ]]; then
        print_warn "$(_t "$CONFIG_FILE 不存在" "$CONFIG_FILE missing")"
        return 1
    fi
    if ! jq empty "$CONFIG_FILE" 2>/dev/null; then
        print_fail "$(_t 'config.json 非合法 JSON' 'config.json not valid JSON')"; return 1
    fi
    print_info "$(_t '以下为 config.json 内容，ApiKey 已隐藏:' 'config.json content (ApiKey masked):')"
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
                "CertFile":   "/etc/yunzes-node/certs/vless-1.crt",
                "KeyFile":    "/etc/yunzes-node/certs/vless-1.key",
                "Email":      "admin@example.com",
                "Provider":   "",
                "RenewBeforeDays": 30
            }
        }
    ]
}
JSON
    print_ok "$(_t "已写入模板: $dest" "Template written: $dest")"
}

cmd_gen_config() {
    ensure_dirs
    if [[ -f "$CONFIG_FILE" ]] && ! confirm "$(_t "$CONFIG_FILE 已存在，是否覆盖（会先备份）" "$CONFIG_FILE exists — overwrite (with backup)")" n; then
        return 0
    fi
    [[ -f "$CONFIG_FILE" ]] && backup_now >/dev/null

    print_info "$(_t '请选择安装模式:' 'Choose install mode:')"
    print_choice "1)" "$(_t '智能模式 — 由面板提供节点列表 (推荐, 需要面板支持 /v2/server/{ID})' 'Smart — panel provides protocol list (recommended, requires /v2/server/{ID})')"
    print_choice "2)" "$(_t '手动模式 — 逐个输入 NodeID + NodeType' 'Manual — enter NodeID + NodeType for each node')"
    local mode
    mode=$(prompt_read "$(_t '请选择' 'Select')" "1")
    case "$mode" in
        2)
            gen_config_template "$CONFIG_FILE"
            if confirm "$(_t '进入交互生成多节点配置' 'Enter multi-node interactive generation')" y; then
                gen_config_interactive
            fi
            ;;
        *)
            local rc
            gen_config_smart_mode
            rc=$?
            case "$rc" in
                0) return 0 ;;
                2)
                    print_info "$(_t '回退到手动模式' 'Falling back to manual mode')"
                    gen_config_template "$CONFIG_FILE"
                    if confirm "$(_t '进入交互生成多节点配置' 'Enter multi-node interactive generation')" y; then
                        gen_config_interactive
                    fi
                    ;;
                *) return 1 ;;
            esac
            ;;
    esac
}

# ============================================================================
# M15 config-gen: 3-tier config generation
#   1. gen_config_smart_mode      probe /v2/server/{id}; write panel-driven config
#   2. gen_config_probe_v1_mode   if /v2 missing, probe /v1/server/config per
#                                 (NodeID, protocol) and only keep 200s
#   3. gen_config_interactive     manual NodeID+NodeType entry (last resort)
# ============================================================================
# gen_config_smart_mode: probe the panel's /v2/server/{ServerId} endpoint
# and write a StartNodes-style config (top-level Api block + empty Nodes
# array). yunzes-node's cmd/server.go branches on this exact shape:
# `len(c.NodeConfig) == 0` plus a populated ApiConfig sends startup down
# the panel-driven path that fetches all protocols at once.
#
# Returns:
#   0 — smart-mode config written, caller is done
#   1 — fatal (user cancelled or unrecoverable error)
#   2 — panel does not support /v2 (404); caller should fall back to
#       gen_config_interactive (manual mode)
gen_config_smart_mode() {
    local api_host secret_key
    api_host=$(prompt_required "$(_t '面板 ApiHost（例: https://api.example.com）' 'Panel ApiHost (e.g. https://api.example.com)')" \
                                "ApiHost 不能为空" "ApiHost is required") || return 1
    secret_key=$(prompt_required "$(_t '面板 SecretKey' 'Panel SecretKey')" \
                                "SecretKey 不能为空，否则面板调用全部 401" "SecretKey is required, otherwise all panel calls return 401") || return 1

    # Multi-ServerID loop: each round probes one ServerID via /v2/server/{id}
    # and accumulates (server_id, protocol_type) pairs. The operator can
    # type "q" or answer N to "再添加一个 ServerID" to stop.
    local -a all_sids=() all_ptypes=() all_pports=()
    local round=1
    local v1_fallback_ran=0

    while true; do
        local sid_prompt sid_default
        if (( round == 1 )); then
            sid_prompt=$(_t 'ServerID（整数，一个 ServerID 可包含多个协议）' 'ServerID (integer, one ServerID can host multiple protocols)')
            sid_default=""
        else
            print_separator
            if ! confirm "$(_t '再添加一个 ServerID（多 ServerID 配置会自动写为 Nodes[] 多节点格式）' \
                              'Add another ServerID (multi-ServerID writes as Nodes[] format)')" n; then
                break
            fi
            sid_prompt=$(_t '下一个 ServerID' 'Next ServerID')
            # Default to the next number after the largest seen ServerID.
            local last_sid="${all_sids[-1]:-1}"
            sid_default=$((last_sid + 1))
        fi
        local sid
        if [[ -n "$sid_default" ]]; then
            sid=$(prompt_read "$sid_prompt" "$sid_default")
        else
            sid=$(prompt_required "$sid_prompt" "ServerID 不能为空" "ServerID is required") || {
                if (( round == 1 )); then return 1; else break; fi
            }
        fi
        if ! [[ "$sid" =~ ^[0-9]+$ ]]; then
            print_warn "$(_t "ServerID 必须是整数，已跳过：$sid" "ServerID must be an integer, skipping: $sid")"
            continue
        fi

        # Probe /v2/server/{sid}
        print_step "$(_t "探测面板 /v2/server/$sid 接口" "Probing panel /v2/server/$sid")"
        local probe_url="${api_host%/}/v2/server/${sid}?secret_key=${secret_key}"
        local probe_log_safe="${api_host%/}/v2/server/${sid}?secret_key=***"
        print_cmd "curl -s --connect-timeout 5 $probe_log_safe"

        local body_file http_code
        body_file=$(mktemp)
        http_code=$(curl -s -o "$body_file" -w '%{http_code}' --connect-timeout 5 "$probe_url" || echo 000)

        case "$http_code" in
            200) ;;
            404)
                # First-round 404: fall back to /v1 probe path.
                # Subsequent-round 404: just warn and let user pick another.
                if (( round == 1 )); then
                    print_warn "$(_t "面板不支持 /v2/server/$sid 接口（HTTP 404）— 切换到 /v1 探测模式" \
                                    "Panel does not implement /v2/server/$sid (HTTP 404) — switching to /v1 probe mode")"
                    rm -f "$body_file"
                    gen_config_probe_v1_mode "$api_host" "$secret_key" "$sid"
                    v1_fallback_ran=1
                    return $?
                else
                    print_warn "$(_t "ServerID=$sid 返回 HTTP 404（面板上不存在），跳过" "ServerID=$sid returned HTTP 404 (not on panel), skipping")"
                    rm -f "$body_file"
                    round=$((round+1))
                    continue
                fi
                ;;
            401|403)
                print_fail "$(_t "面板拒绝（HTTP $http_code）— SecretKey 错或权限不足" "Panel rejected (HTTP $http_code) — wrong SecretKey or insufficient privilege")"
                rm -f "$body_file"
                return 1
                ;;
            000|"")
                print_fail "$(_t "无法连接面板 $api_host" "Cannot connect to panel $api_host")"
                rm -f "$body_file"
                return 1
                ;;
            5*)
                print_warn "$(_t "面板内部错误 HTTP $http_code，跳过该 ServerID" "Panel internal error HTTP $http_code, skipping ServerID")"
                rm -f "$body_file"
                round=$((round+1))
                continue
                ;;
            *)
                print_warn "$(_t "面板返回非预期状态 HTTP $http_code，将尝试解析" "Panel returned unexpected status HTTP $http_code, trying to parse anyway")"
                ;;
        esac

        if ! jq empty "$body_file" 2>/dev/null; then
            print_warn "$(_t "ServerID=$sid 返回非合法 JSON，跳过" "ServerID=$sid returned invalid JSON, skipping")"
            rm -f "$body_file"
            round=$((round+1))
            continue
        fi
        local protocols_count
        protocols_count=$(jq '.data.protocols | length // 0' "$body_file" 2>/dev/null)
        if (( protocols_count == 0 )); then
            print_warn "$(_t "ServerID=$sid 上没有配置任何协议，跳过" "ServerID=$sid has zero protocols configured, skipping")"
            rm -f "$body_file"
            round=$((round+1))
            continue
        fi

        print_ok "$(_t "ServerID=$sid 返回 $protocols_count 个协议:" "ServerID=$sid returned $protocols_count protocols:")"
        local row
        while IFS= read -r row; do
            local ptype pport psec
            ptype=$(echo "$row" | jq -r '.type // "?"')
            pport=$(echo "$row" | jq -r '.port // "?"')
            psec=$( echo "$row" | jq -r '.security // ""')
            if [[ -n "$psec" && "$psec" != "null" ]]; then
                print_choice "  +" "ServerID=$sid  $ptype  port=$pport  security=$psec"
            else
                print_choice "  +" "ServerID=$sid  $ptype  port=$pport"
            fi
            all_sids+=("$sid")
            all_ptypes+=("$ptype")
            all_pports+=("$pport")
        done < <(jq -c '.data.protocols[]' "$body_file")
        rm -f "$body_file"
        round=$((round+1))
    done

    if (( v1_fallback_ran == 1 )); then
        return 0  # v1-probe-mode path already wrote the config
    fi

    local total_count="${#all_sids[@]}"
    if (( total_count == 0 )); then
        print_fail "$(_t '没有探测到任何协议' 'No protocols discovered')"
        return 1
    fi

    print_separator
    print_ok "$(_t "总计探测到 $total_count 个协议" "Discovered $total_count protocols total")"
    print_separator

    if ! confirm "$(_t "确认使用以上 $total_count 个协议生成配置" \
                       "Confirm generating config from these $total_count protocols")" y; then
        return 1
    fi

    # Output format selection:
    #   - 1 unique ServerID  -> StartNodes-style config (panel-driven /v2 at
    #     runtime; one /v2 call per refresh; most efficient)
    #   - >1 unique ServerID -> Nodes[] config (one entry per (sid,protocol);
    #     runtime fetches each via /v1)
    local unique_sids
    unique_sids=$(printf '%s\n' "${all_sids[@]}" | sort -u | wc -l)

    local final
    if (( unique_sids == 1 )); then
        local only_sid="${all_sids[0]}"
        final=$(jq -n \
            --arg ah "$api_host" --arg sk "$secret_key" --argjson sid "$only_sid" \
            '{
                Log:   {Level: "info"},
                Cores: [{Type:"xray"}, {Type:"sing"}],
                Api:   {ApiHost: $ah, ServerID: $sid, SecretKey: $sk, Timeout: 30},
                Nodes: []
            }')
        print_info "$(_t "单 ServerID 配置 → 智能模式 config（运行时一次 /v2/server/$only_sid 拿全部 $total_count 个协议）" \
                       "Single ServerID -> smart-mode config (runtime: one /v2/server/$only_sid call returns all $total_count protocols)")"
    else
        local nodes_json="[]" i
        for ((i=0; i<total_count; i++)); do
            local node
            node=$(jq -n \
                --arg ah "$api_host" --arg ak "$secret_key" \
                --argjson nid "${all_sids[$i]}" --arg nt "${all_ptypes[$i]}" \
                '{ApiHost:$ah, ApiKey:$ak, NodeID:$nid, NodeType:$nt, Timeout:30, ListenIP:"0.0.0.0", CertConfig:{CertMode:"none"}}')
            nodes_json=$(echo "$nodes_json" | jq --argjson n "$node" '. + [$n]')
        done
        final=$(jq -n --argjson nodes "$nodes_json" '{
            Log:   {Level: "info"},
            Cores: [{Type:"xray"}, {Type:"sing"}],
            Nodes: $nodes
        }')
        print_info "$(_t "多 ServerID（$unique_sids 个） → Nodes[] 多节点 config（每个 (ServerID, 协议) 一条记录）" \
                       "Multiple ServerIDs ($unique_sids) -> Nodes[] config (one entry per (ServerID, protocol))")"
    fi

    if ! echo "$final" | jq empty 2>/dev/null; then
        print_fail "$(_t '生成的 JSON 不合法（脚本 bug，请反馈）' 'Generated JSON invalid (script bug)')"
        return 1
    fi
    echo "$final" > "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    print_ok "$(_t "已写入 $CONFIG_FILE （$total_count 个协议）" "Wrote $CONFIG_FILE ($total_count protocols)")"
    print_info "$(_t '运行时面板会决定每个协议的端口、加密方式、TLS 等具体参数；本地不再需要手动维护协议列表' \
                    'At runtime panel decides per-protocol port/cipher/TLS; local config no longer needs manual protocol lists')"
    return 0
}

# gen_config_probe_v1_mode: panel doesn't implement /v2/server/{id}, so we
# fall back to per-protocol probing via /v1/server/config?protocol=X&
# server_id=Y. The operator gives a list of NodeIDs (their actual panel
# server IDs), and the script curls all 7 supported protocols against each
# one. Only the (NodeID, protocol) pairs that return HTTP 200 land in the
# generated Nodes[] array. The operator NEVER types NodeType / CertMode
# manually — discovery is fully panel-driven.
#
# Returns 0 on success (config written), 1 on failure (panel unreachable
# or operator cancelled).
gen_config_probe_v1_mode() {
    local api_host="$1" secret_key="$2" default_sid="$3"

    print_info "$(_t '请列出你的面板上配置过的 NodeID（逗号分隔，例: 1,2,5,9）。脚本会自动探测每个 NodeID 上配的协议。' \
                    'List the NodeIDs configured on your panel (comma-separated, e.g. 1,2,5,9). The script will probe each NodeID for all supported protocols automatically.')"
    local raw
    raw=$(prompt_required "$(_t "NodeID 列表（默认 $default_sid）" "NodeID list (default $default_sid)")" \
                          "至少需要一个 NodeID" "At least one NodeID required") || {
        print_warn "$(_t '已取消探测' 'Probe cancelled')"
        return 1
    }
    [[ -z "$raw" ]] && raw="$default_sid"
    raw="${raw// /}"  # strip spaces

    local probe_protocols=(vless vmess trojan shadowsocks hysteria2 tuic anytls)
    local nodes_json="[]"
    local found=0 net_fail=0
    local sid proto code

    while IFS=',' read -ra _SIDS <<<"$raw"; do
        for sid in "${_SIDS[@]}"; do
            [[ -z "$sid" ]] && continue
            if ! [[ "$sid" =~ ^[0-9]+$ ]]; then
                print_warn "$(_t "跳过非数字 NodeID: $sid" "Skipping non-numeric NodeID: $sid")"
                continue
            fi
            print_step "$(_t "探测 NodeID=$sid 上的协议" "Probing NodeID=$sid for protocols")"
            for proto in "${probe_protocols[@]}"; do
                local url="${api_host%/}/v1/server/config?protocol=${proto}&server_id=${sid}&secret_key=${secret_key}"
                code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$url" 2>/dev/null || echo 000)
                case "$code" in
                    200)
                        print_ok "$(_t "  ✓ NodeID=$sid 协议=$proto" "  ✓ NodeID=$sid protocol=$proto")"
                        found=$((found+1))
                        local node
                        node=$(jq -n \
                            --arg ah "$api_host" --arg ak "$secret_key" \
                            --argjson nid "$sid" --arg nt "$proto" \
                            '{ApiHost:$ah, ApiKey:$ak, NodeID:$nid, NodeType:$nt, Timeout:30, ListenIP:"0.0.0.0", CertConfig:{CertMode:"none"}}')
                        nodes_json=$(echo "$nodes_json" | jq --argjson n "$node" '. + [$n]')
                        ;;
                    404|"") ;;
                    401|403)
                        print_warn "$(_t "  $proto: HTTP $code (鉴权失败 — SecretKey 错或权限不足)" "  $proto: HTTP $code (auth failed)")"
                        ;;
                    000)
                        print_fail "$(_t "  无法连接面板 $api_host — 终止探测" "  Cannot reach panel $api_host — abort probe")"
                        net_fail=1
                        break 2
                        ;;
                    *)
                        print_info "$(_t "  $proto: HTTP $code" "  $proto: HTTP $code")"
                        ;;
                esac
            done
        done
        break
    done

    if (( net_fail )); then
        return 1
    fi
    if (( found == 0 )); then
        print_fail "$(_t "在给定的 NodeID 上没有探测到任何协议" "No protocols discovered on the given NodeIDs")"
        print_fix "$(_t "请确认面板后台真的配了节点；或换一个 NodeID 范围" "Verify the panel has nodes configured; or try different NodeIDs")"
        return 1
    fi

    print_separator
    print_ok "$(_t "探测完成：发现 $found 个有效节点" "Probe complete: $found valid nodes")"
    echo "$nodes_json" | jq -r '.[] | "  NodeID=\(.NodeID)  协议=\(.NodeType)"'
    print_separator

    if ! confirm "$(_t "确认使用以上 $found 个节点（CertMode 默认 none，可日后用 yunzes-node edit-config 单独加 TLS）" \
                       "Confirm using $found nodes (CertMode defaults to none; add TLS later via yunzes-node edit-config)")" y; then
        return 1
    fi

    local final
    final=$(jq -n --argjson nodes "$nodes_json" '{
        Log: {Level: "info"},
        Cores: [{Type: "xray"}, {Type: "sing"}],
        Nodes: $nodes
    }')
    echo "$final" > "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    print_ok "$(_t "已写入 $CONFIG_FILE （/v1 探测模式：$found 个节点，全部 CertMode=none）" \
                  "Wrote $CONFIG_FILE (v1 probe mode: $found nodes, all CertMode=none)")"
    print_warn "$(_t "若节点本身需要 TLS（hysteria2 / tuic / anytls / vless+tls）：用 yunzes-node edit-config 给该节点的 CertConfig 加 CertMode=self/http + CertDomain；当前 cleartext 模式下 TLS 协议会跑但客户端无法验证证书" \
                    "If a node requires TLS (hysteria2 / tuic / anytls / vless+tls): use yunzes-node edit-config to add CertMode=self/http + CertDomain to that node's CertConfig; under current cleartext mode TLS protocols will run but clients cannot verify the cert")"
    return 0
}

# gen_config_interactive: now enforces non-empty CertDomain when CertMode
# requires it, defaulting to a node-keyed cert path with a hyphen separator
# (vless-1.crt vs the previous vless1.crt) for readability.
gen_config_interactive() {
    print_info "$(_t '进入交互式配置生成 — Ctrl-C 中途退出会保留模板状态' 'Entering interactive generation — Ctrl-C exits early, template state preserved')"
    echo
    local nodes_json="[]"
    while true; do
        local idx
        idx=$(echo "$nodes_json" | jq 'length')
        print_title "$(_t "--- 添加节点 #${idx} ---" "--- Add node #${idx} ---")"
        local api_host api_key node_id node_type listen_ip timeout
        local cert_mode cert_domain cert_file key_file email
        api_host=$(prompt_read "ApiHost"      "https://your-panel.example.com")
        api_key=$(prompt_required "ApiKey" \
            "ApiKey 不能为空，否则面板调用全部 401" \
            "ApiKey is required, otherwise all panel calls return 401")
        node_id=$(prompt_read  "NodeID" "1")
        print_info "$(_t '支持协议: vless / vmess / trojan / shadowsocks / hysteria2 / tuic / anytls' \
                       'Supported protocols: vless / vmess / trojan / shadowsocks / hysteria2 / tuic / anytls')"
        node_type=$(prompt_read "NodeType" "vless")
        listen_ip=$(prompt_read "ListenIP" "0.0.0.0")
        timeout=$(prompt_read   "Timeout"  "30")

        local need_tls=0
        case "$node_type" in
            hysteria2|tuic|anytls) need_tls=1 ;;
            vless|vmess|trojan)
                if confirm "$(_t '该协议是否启用 TLS (reality / 无加密回答 N)' 'Enable TLS for this protocol (reply N for reality / cleartext)')" y; then
                    need_tls=1
                fi ;;
        esac

        local cert_obj="null"
        if (( need_tls )); then
            print_info "$(_t 'CertMode 选项: http (ACME HTTP-01) / dns (ACME DNS-01) / file (你提供) / self (自签) / none' \
                           'CertMode options: http / dns / file / self / none')"
            cert_mode=$(prompt_read "CertMode" "self")
            case "$cert_mode" in
                http|dns|file|self)
                    local _cancelled=0
                    if ! cert_domain=$(prompt_required "CertDomain" \
                        "CertDomain 不能为空，否则 EnsureCertificate 会拒绝并触发容器重启循环。如需无加密，请输入 q 回退到 CertMode=none" \
                        "CertDomain must be non-empty, otherwise EnsureCertificate rejects it. Type q to fall back to CertMode=none"); then
                        print_warn "$(_t "用户取消 CertDomain — 该节点回退到 CertMode=none（cleartext）" \
                                        "User cancelled CertDomain — falling back to CertMode=none (cleartext)")"
                        cert_obj='{"CertMode":"none"}'
                        _cancelled=1
                    fi
                    if (( ! _cancelled )); then
                        cert_file=$(prompt_read   "CertFile"  "/etc/yunzes-node/certs/${node_type}-${node_id}.crt")
                        key_file=$(prompt_read    "KeyFile"   "/etc/yunzes-node/certs/${node_type}-${node_id}.key")
                        if [[ "$cert_mode" =~ ^(http|dns)$ ]]; then
                            if ! email=$(prompt_required "Email (ACME 注册用)" \
                                "Email 不能为空（ACME 必填），输入 q 回退到自签证书" \
                                "Email is required for ACME, type q to fall back to self-signed"); then
                                print_warn "$(_t "用户取消 Email — 该节点 CertMode 改为 self（自签）" \
                                                "User cancelled Email — switching CertMode to self for this node")"
                                cert_mode="self"
                            fi
                        else
                            email=""
                        fi
                        cert_obj=$(jq -n \
                            --arg m "$cert_mode" --arg d "$cert_domain" \
                            --arg cf "$cert_file" --arg kf "$key_file" --arg e "${email:-}" \
                            '{CertMode:$m, CertDomain:$d, CertFile:$cf, KeyFile:$kf, Email:$e, RenewBeforeDays:30}')
                    fi
                    ;;
                none|"") cert_obj='{"CertMode":"none"}' ;;
                *) print_warn "$(_t "未知 CertMode '$cert_mode'，回退到 none" "Unknown CertMode '$cert_mode', falling back to none")"
                   cert_obj='{"CertMode":"none"}' ;;
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
        if ! confirm "$(_t '继续添加下一个节点' 'Add another node')" n; then break; fi
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
        print_ok "$(_t "已写入 $CONFIG_FILE" "Written: $CONFIG_FILE")"
        # Run validate_config so the operator sees pass/fail right after gen.
        validate_config || true
    else
        print_fail "$(_t '生成的 JSON 不合法 (脚本 bug, 请反馈)' 'Generated JSON invalid (script bug, please report)')"
        return 1
    fi
}

cmd_check_panel() {
    [[ -f "$CONFIG_FILE" ]] || { print_fail "$(_t '无 config.json' 'No config.json')"; return 1; }
    jq empty "$CONFIG_FILE" 2>/dev/null || { print_fail "$(_t 'config.json 非合法 JSON' 'config.json not valid JSON')"; return 1; }
    preflight_panel_check
}

cmd_ports() {
    if ! command -v ss >/dev/null 2>&1; then
        print_fail "$(_t '缺少 ss 命令; apt install -y iproute2' 'ss missing; apt install -y iproute2')"
        return 1
    fi
    if container_running; then
        local pid
        pid=$(docker inspect --format '{{.State.Pid}}' "$NAME" 2>/dev/null || true)
        print_info "$(_t "yunzes-node PID = $pid (host network 下与宿主端口表合并)" \
                        "yunzes-node PID = $pid (host network shares the host listen table)")"
        print_info "$(_t '以下为 ss -lntup 过滤后的原始输出:' 'Raw filtered ss -lntup output below:')"
        ss -lntup | awk -v p="$pid" 'NR==1 || $0 ~ p'
    else
        print_warn "$(_t '容器未运行 — 显示宿主当前所有监听' 'Container not running — showing all host listeners')"
        ss -lntup
    fi
}

cmd_containers() {
    print_info "$(_t '以下为 docker ps 原始输出:' 'Raw docker ps output below:')"
    docker ps -a --filter "name=^${NAME}$" --no-trunc
    echo
    print_info "$(_t '以下为 docker inspect 头 80 行原始输出:' 'Raw docker inspect head 80 lines below:')"
    docker inspect "$NAME" 2>/dev/null | head -80 || print_warn "$(_t '容器不存在' 'Container missing')"
}

cmd_backup() {
    ensure_dirs
    local b
    b=$(backup_now)
    print_ok "$(_t "备份完成: $b" "Backup complete: $b")"
    print_info "$(_t '以下为 ls -lh 原始输出:' 'Raw ls -lh output below:')"
    ls -lh "$b"
    echo
    print_info "$(_t '回滚命令: yunzes-node rollback (或菜单 18)' 'Rollback: yunzes-node rollback (or menu 18)')"
}

cmd_rollback() {
    local backups
    mapfile -t backups < <(list_backups)
    if (( ${#backups[@]} == 0 )); then
        print_warn "$(_t '没有可用的备份' 'No backups available')"
        return 0
    fi
    print_info "$(_t '可回滚的备份:' 'Available backups:')"
    local i
    for ((i=0; i<${#backups[@]}; i++)); do
        print_choice "$((i+1)))" "${backups[$i]}"
    done
    print_choice "q)" "$(_t '取消' 'Cancel')"
    local choice
    choice=$(prompt_read "$(_t "选择 [1-${#backups[@]}, q]" "Choose [1-${#backups[@]}, q]")" "q")
    [[ "$choice" =~ ^[Qq]$ ]] && { print_info "$(_t '已取消' 'Cancelled')"; return 0; }
    if ! [[ "$choice" =~ ^[0-9]+$ ]] || (( choice < 1 || choice > ${#backups[@]} )); then
        print_fail "$(_t '无效选项' 'Invalid option')"; return 1
    fi
    local target="${backups[$((choice-1))]}"
    print_danger "$(_t "回滚到 $target — 将覆盖当前 config.json + certs/ + 容器" "Rollback to $target — will overwrite current config.json + certs/ + container")"
    print_warn   "$(_t '此操作不可直接撤销, 但回滚前会自动再做一次安全备份' 'Not directly reversible, but a safety backup is taken first')"
    if ! confirm "$(_t '继续' 'Continue')" n; then
        print_info "$(_t '已取消' 'Cancelled')"; return 0
    fi
    local before
    before=$(backup_now)
    print_info "$(_t "当前状态已备份到 $before（回滚前保险快照）" "Current state backed up to $before (rollback-before safety)")"
    if restore_from_backup "$target"; then
        sleep 3
        cmd_verify || print_warn "$(_t 'verify 有未通过项' 'verify has issues')"
    else
        print_fail "$(_t "回滚失败 — yunzes-node status 查看容器; $before 是回滚前的备份" \
                        "Rollback failed — check yunzes-node status; $before holds the pre-rollback state")"
        return 1
    fi
}

cmd_cleanup_images() {
    print_info "$(_t '悬空镜像 (dangling):' 'Dangling images:')"
    docker images --filter "dangling=true" --format "table {{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}"
    echo
    print_danger "$(_t '下面的清理操作会删除所有 dangling 镜像 (容器进行中的镜像不会被影响)' \
                      'Cleanup will delete all dangling images (in-use images are unaffected)')"
    if confirm "$(_t '继续清理 dangling 镜像' 'Proceed with dangling-image prune')" n; then
        print_cmd "docker image prune -f"
        docker image prune -f
        print_ok "$(_t '清理完成' 'Cleanup complete')"
    fi
    echo
    print_info "$(_t '全部 yunzes-node 历史镜像 (含 untagged):' 'All historical yunzes-node images (incl. untagged):')"
    docker images --filter "reference=yunzes-node" --format "table {{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}"
}

# ============================================================================
# M20 panel-tools: cmd_check_panel / cmd_ports / cmd_containers / cmd_backup /
#                  cmd_rollback / cmd_cleanup_images.
# (cmd_check_panel reuses M13 preflight_panel_check.)
# ============================================================================
# (definitions follow M21 below; placement preserved for git-blame stability.)

# ============================================================================
# M21 fake-panel: bundled Python /v1+/v2 fake panel + cmd_fake_test
# orchestration (4-protocol bring-up + verify + cert-reuse + cleanup).
# ============================================================================
write_fake_panel_py() {
    cat > "$FAKE_PANEL_FILE" <<'PYEOF'
#!/usr/bin/env python3
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
        "security_config": {"sni": "vless.test"}}},
    ("shadowsocks", "102"): {"basic": BASIC, "protocol": "shadowsocks", "config": {
        "port": 8102, "method": "aes-256-gcm"}},
    ("hysteria2", "103"): {"basic": BASIC, "protocol": "hysteria2", "config": {
        "port": 8103, "up_mbps": 100, "down_mbps": 100, "obfs_password": "obfs-secret",
        "security_config": {"sni": "hy2.test"}}},
    ("vless", "104"): {"basic": BASIC, "protocol": "vless", "config": {
        "port": 8104, "transport": "tcp", "security": "reality",
        "security_config": {
            "sni": "reality.test",
            "reality_server_addr": "www.cloudflare.com",
            "reality_server_port": 443,
            "reality_private_key": "wEbNI8QwM1XLgX-ucy7Qwp6msGmGCfSMQClC-VRjV3w",
            "reality_short_id": "0123456789abcdef"}}},
}
class Handler(BaseHTTPRequestHandler):
    def _send_json(self, body, status=200):
        b = json.dumps(body).encode()
        self.send_response(status); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        u = urlparse(self.path); q = parse_qs(u.query)
        proto = q.get("protocol", [""])[0]; sid = q.get("server_id", [""])[0]
        if u.path == "/v1/server/config":
            cfg = NODE_TABLE.get((proto, sid))
            if cfg is None: return self._send_json({"error": f"no node for {proto}/{sid}"}, 404)
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
        print_info "$(_t "模拟面板已在运行 (PID $(<"$FAKE_PANEL_PID_FILE"))" "fake panel already running (PID $(<"$FAKE_PANEL_PID_FILE"))")"
        return 0
    fi
    write_fake_panel_py
    print_cmd "python3 $FAKE_PANEL_FILE > $FAKE_PANEL_LOG_FILE 2>&1 &"
    nohup python3 "$FAKE_PANEL_FILE" >"$FAKE_PANEL_LOG_FILE" 2>&1 &
    echo $! > "$FAKE_PANEL_PID_FILE"
    sleep 1
    if curl -s --max-time 2 "http://127.0.0.1:${FAKE_PANEL_PORT}/v1/server/user" >/dev/null; then
        print_ok "$(_t "模拟面板启动成功 (PID $(<"$FAKE_PANEL_PID_FILE"))" "fake panel started (PID $(<"$FAKE_PANEL_PID_FILE"))")"
    else
        print_fail "$(_t "模拟面板启动失败 — 查看 $FAKE_PANEL_LOG_FILE" "fake panel start failed — see $FAKE_PANEL_LOG_FILE")"
        return 1
    fi
}

write_fake_test_config() {
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
    local effective_restart="no"

    if ! command -v python3 >/dev/null 2>&1; then
        print_fail "$(_t '缺少 python3 — apt install -y python3' 'python3 missing — apt install -y python3')"
        return 1
    fi
    if ! command -v curl >/dev/null 2>&1; then
        print_fail "$(_t '缺少 curl' 'curl missing')"; return 1
    fi
    detect_docker_state >/dev/null
    case $? in
        0) ;;
        10) print_fail "$(_t 'Docker 未安装' 'Docker not installed')"; return 1 ;;
        11) print_fail "$(_t 'Docker daemon 未运行' 'Docker daemon not running')"; return 1 ;;
        12) print_fail "$(_t '无 docker socket 权限' 'No docker socket permission')"; return 1 ;;
    esac
    if ! docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        print_fail "$(_t "镜像 $DEFAULT_IMAGE 不存在; 先 yunzes-node install 或 docker build" "Image $DEFAULT_IMAGE missing; run yunzes-node install or docker build first")"
        return 1
    fi

    ensure_dirs
    mkdir -p "$FAKE_CERTS_DIR"
    chmod 750 "$FAKE_CERTS_DIR" 2>/dev/null || true

    local pre_backup=""
    if [[ -f "$CONFIG_FILE" ]]; then
        pre_backup=$(backup_now)
        print_info "$(_t "fake-test 前已备份原配置: $pre_backup" "Pre-fake-test backup: $pre_backup")"
    fi
    write_fake_test_config
    cp -f "$FAKE_TEST_CONFIG" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    print_ok "$(_t "已写入 4 协议测试配置 → $CONFIG_FILE" "Wrote 4-protocol test config → $CONFIG_FILE")"

    start_fake_panel || return 1

    print_cmd "docker rm -f $NAME"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    print_step "$(_t "启动测试容器 (restart=$effective_restart)" "Starting test container (restart=$effective_restart)")"
    if ! _docker_run "$DEFAULT_IMAGE" "$effective_restart"; then
        print_fail "$(_t '测试容器启动失败' 'Test container start failed')"
        [[ -n "$pre_backup" && -f "$pre_backup/config.json" ]] \
            && cp -p "$pre_backup/config.json" "$CONFIG_FILE"
        return 1
    fi

    print_step "$(_t '等待 5 秒...' 'Waiting 5s...')"; sleep 5
    echo

    local logs fake_pass=0 fake_fail=0
    logs="$(docker logs "$NAME" 2>&1 || true)"

    local _bad
    _bad=$(echo "$logs" | grep -E -i 'panic|nil pointer|runtime error|segmentation violation' | head -3 || true)
    if [[ -z "$_bad" ]]; then
        print_ok "$(_t '无 panic / nil pointer / runtime error' 'No panic / nil pointer / runtime error')"; fake_pass=$((fake_pass+1))
    else
        print_fail "$(_t '发现致命错误:' 'Fatal error found:')"
        print_info "$(_t '以下为匹配到的原始日志行:' 'Matching raw log lines:')"
        echo "$_bad" | sed 's/^/      /'
        fake_fail=$((fake_fail+1))
    fi
    local marker
    for marker in "Core Selector" "Adding node inbound" "logical_tag" "core=" "runtime_key" "protocol=" "server_id" "port="; do
        if echo "$logs" | grep -qF "$marker"; then
            print_ok "$(_t "日志含字段: $marker" "log contains: $marker")"; fake_pass=$((fake_pass+1))
        else
            print_fail "$(_t "日志未见: $marker" "log missing: $marker")"; fake_fail=$((fake_fail+1))
        fi
    done

    if command -v ss >/dev/null 2>&1; then
        echo
        print_step "$(_t '端口监听检查 (host network)' 'Listen check (host network)')"
        local check port proto hits
        for check in "8101 tcp" "8102 tcp" "8102 udp" "8103 udp" "8104 tcp"; do
            read -r port proto <<<"$check"
            case "$proto" in
                tcp) hits=$(ss -lntp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
                udp) hits=$(ss -lnup 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"' || true) ;;
            esac
            if [[ -n "$hits" ]] && echo "$hits" | grep -q yunzes-node; then
                print_ok "$(_t "$port/$proto 由 yunzes-node 监听" "$port/$proto owned by yunzes-node")"; fake_pass=$((fake_pass+1))
            else
                print_fail "$(_t "$port/$proto 未由 yunzes-node 监听" "$port/$proto NOT owned by yunzes-node")"; fake_fail=$((fake_fail+1))
            fi
        done
    else
        print_warn "$(_t '缺少 ss, 跳过端口监听检查' 'ss missing, skipping listen check')"
    fi

    echo
    print_step "$(_t '证书复用检查 (restart 后应 reuse)' 'Cert reuse check (restart should reuse)')"
    print_cmd "docker restart $NAME"
    docker restart "$NAME" >/dev/null 2>&1 || true
    sleep 3
    local cert_lines
    cert_lines=$(docker logs --tail 200 "$NAME" 2>&1 | grep cert_action || true)
    if echo "$cert_lines" | grep -q 'cert_action=reuse'; then
        print_ok "$(_t '重启后 cert_action=reuse (C3 持久化生效)' 'cert_action=reuse after restart (C3 persistence works)')"; fake_pass=$((fake_pass+1))
    else
        print_warn "$(_t '未在重启日志里找到 cert_action=reuse' 'cert_action=reuse not seen in restart logs')"
        print_info "$(_t '以下为 cert_action 相关日志原文:' 'cert_action raw lines below:')"
        echo "$cert_lines" | sed 's/^/      /'
    fi

    echo
    print_separator
    printf "$(_t '模拟面板验证汇总:' 'fake-test summary:') %bPASS=%d%b  %bFAIL=%d%b\n" \
        "${C_GREEN}" "$fake_pass" "${C_PLAIN}" "${C_RED}" "$fake_fail" "${C_PLAIN}"
    print_separator

    echo
    if confirm "$(_t '停止模拟面板' 'Stop fake panel')" y; then
        cmd_stop_fake_panel
    fi
    if confirm "$(_t "删除测试容器 ($NAME)" "Delete test container ($NAME)")" n; then
        print_cmd "docker rm -f $NAME"
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        print_ok "$(_t '测试容器已删除' 'Test container deleted')"
    fi
    if confirm "$(_t "保留测试证书 ($FAKE_CERTS_DIR)" "Keep test certs ($FAKE_CERTS_DIR)")" n; then
        print_info "$(_t "保留 — 测试证书隔离在 $FAKE_CERTS_DIR, 不影响真实 $CERTS_DIR" \
                        "Keeping — test certs isolated under $FAKE_CERTS_DIR; real $CERTS_DIR untouched")"
    else
        print_cmd "rm -rf $FAKE_CERTS_DIR"
        rm -rf "$FAKE_CERTS_DIR"
        print_ok "$(_t '测试证书目录已删除' 'Test cert dir deleted')"
    fi
    echo
    if [[ -n "$pre_backup" ]]; then
        if confirm "$(_t "恢复 fake-test 之前的 config.json (来自 $pre_backup)" "Restore pre-fake-test config.json (from $pre_backup)")" y; then
            if [[ -f "$pre_backup/config.json" ]]; then
                cp -p "$pre_backup/config.json" "$CONFIG_FILE"
                print_ok "$(_t "已恢复: $pre_backup/config.json" "Restored: $pre_backup/config.json")"
            else
                print_warn "$(_t "$pre_backup/config.json 不存在; 保留测试 config.json" "$pre_backup/config.json missing; keeping test config.json")"
            fi
        fi
    else
        print_info "$(_t '验证启动前没有 config.json, 无需恢复' 'No prior config.json, nothing to restore')"
    fi
    return $fake_fail
}

cmd_stop_fake_panel() {
    if [[ -f "$FAKE_PANEL_PID_FILE" ]]; then
        local pid
        pid=$(<"$FAKE_PANEL_PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            print_cmd "kill $pid"
            kill "$pid" 2>/dev/null && print_ok "$(_t "模拟面板已停止 (PID $pid)" "fake panel stopped (PID $pid)")"
        fi
        rm -f "$FAKE_PANEL_PID_FILE"
    fi
    pkill -f fake_panel.py 2>/dev/null || true
}

# ============================================================================
# M22 cleanup: cmd_uninstall (preserve data) / cmd_uninstall_full (wipe with
# DELETE-YUNZES-NODE phrase guard) / cmd_setup_entry (install 0755 to
# /usr/bin/yunzes-node).
# ============================================================================
cmd_uninstall() {
    print_danger "$(_t '卸载操作: 将删除容器并可选删除镜像 / 全局命令' 'Uninstall: deletes container, optionally image / global cmd')"
    print_warn   "$(_t "保留: $CONFIG_DIR (含证书) 与 $BACKUP_DIR (备份)" "Preserved: $CONFIG_DIR (incl. certs) and $BACKUP_DIR (backups)")"
    if ! confirm "$(_t '继续' 'Continue')" y; then
        print_info "$(_t '已取消' 'Cancelled')"
        return 0
    fi
    if container_exists; then
        print_cmd "docker rm -f $NAME"
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        print_ok "$(_t '容器已删除' 'Container deleted')"
    fi
    if docker image inspect "$DEFAULT_IMAGE" >/dev/null 2>&1; then
        if confirm "$(_t "同时删除镜像 $DEFAULT_IMAGE" "Also delete image $DEFAULT_IMAGE")" n; then
            print_cmd "docker rmi -f $DEFAULT_IMAGE"
            docker rmi -f "$DEFAULT_IMAGE" >/dev/null 2>&1 && print_ok "$(_t '镜像已删除' 'Image deleted')"
        fi
    fi
    if [[ -f "$INSTALLED_PATH" ]] && confirm "$(_t "删除全局命令 $INSTALLED_PATH" "Delete global command $INSTALLED_PATH")" n; then
        rm -f "$INSTALLED_PATH"
        print_ok "$(_t "$INSTALLED_PATH 已删除" "$INSTALLED_PATH deleted")"
    fi
    print_info "$(_t "保留: $CONFIG_DIR  $CERTS_DIR  $BACKUP_DIR" "Kept: $CONFIG_DIR  $CERTS_DIR  $BACKUP_DIR")"
    print_info "$(_t '如需彻底清理: yunzes-node uninstall-full' 'For full wipe: yunzes-node uninstall-full')"
}

cmd_uninstall_full() {
    print_danger "$(_t '危险操作: 完全卸载 — 将删除以下所有内容' 'DANGER: full uninstall — will delete everything below')"
    print_fail   "$(_t "  - 容器        $NAME" "  - container   $NAME")"
    print_fail   "$(_t "  - 镜像        $DEFAULT_IMAGE" "  - image       $DEFAULT_IMAGE")"
    print_fail   "$(_t "  - 配置目录    $CONFIG_DIR (含证书 + fake-test 证书)" "  - config dir  $CONFIG_DIR (incl. certs + fake-test certs)")"
    print_fail   "$(_t "  - 运行目录    $RUN_DIR (含全部备份 + 源码 $SRC_DIR)" "  - run dir     $RUN_DIR (incl. all backups + source $SRC_DIR)")"
    print_fail   "$(_t "  - 命令入口    $INSTALLED_PATH" "  - global cmd  $INSTALLED_PATH")"
    print_warn   "$(_t '此操作不可恢复' 'This is NOT reversible')"
    print_fix    "$(_t "如需取消, 直接回车或输入除 'DELETE YUNZES NODE' 之外的任何文字" "To cancel, press Enter or type anything other than 'DELETE YUNZES NODE'")"
    echo
    if ! confirm_phrase "$(_t '请输入 DELETE YUNZES NODE 二次确认' 'Type DELETE YUNZES NODE to confirm')" "DELETE YUNZES NODE"; then
        print_info "$(_t '已取消' 'Cancelled')"
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
    print_ok "$(_t '彻底卸载完成' 'Full uninstall complete')"
}

# ============================================================================
# M23 lang: cmd_lang — show or persist the operator language preference.
# (first-run picker lives in M06; lang here is the CLI surface for it.)
# ============================================================================
cmd_lang() {
    # Persist locale to LOCALE_STATE_FILE. Resets are also supported.
    local target="${1:-}"
    if [[ -z "$target" ]]; then
        print_info "$(_t "当前 locale: $LOCALE" "Current locale: $LOCALE")"
        if [[ -f "$LOCALE_STATE_FILE" ]]; then
            print_info "$(_t "持久化偏好: $(<"$LOCALE_STATE_FILE")  (来自 $LOCALE_STATE_FILE)" \
                            "Persisted preference: $(<"$LOCALE_STATE_FILE")  (from $LOCALE_STATE_FILE)")"
        else
            print_info "$(_t "未设置持久偏好（默认中文）；用 yunzes-node lang en 切换并保存" \
                            "No persisted preference (default zh); use 'yunzes-node lang en' to switch and persist")"
        fi
        if [[ -n "${YUNZES_LANG:-}" ]]; then
            print_warn "$(_t "本次环境变量 YUNZES_LANG=$YUNZES_LANG 覆盖了持久偏好" \
                            "YUNZES_LANG=$YUNZES_LANG is overriding the persisted preference for this run")"
        fi
        return 0
    fi
    if ! is_root; then
        print_fail "$(_t "需要 root 才能写 $LOCALE_STATE_FILE" "Need root to write $LOCALE_STATE_FILE")"
        return 1
    fi
    case "$target" in
        zh|cn|chinese|中文)
            mkdir -p "$STATE_DIR"
            echo "zh" > "$LOCALE_STATE_FILE"
            print_ok "$(_t "已切换到中文（持久化到 $LOCALE_STATE_FILE）" "Switched to Chinese (persisted to $LOCALE_STATE_FILE)")"
            ;;
        en|english|英文)
            mkdir -p "$STATE_DIR"
            echo "en" > "$LOCALE_STATE_FILE"
            print_ok "$(_t "Switched to English (persisted to $LOCALE_STATE_FILE)" "Switched to English (persisted to $LOCALE_STATE_FILE)")"
            ;;
        reset|default|清空)
            if [[ -f "$LOCALE_STATE_FILE" ]]; then
                rm -f "$LOCALE_STATE_FILE"
                print_ok "$(_t "已重置为默认（中文）" "Reset to default (Chinese)")"
            else
                print_info "$(_t "已经是默认状态（中文）" "Already default (Chinese)")"
            fi
            ;;
        *)
            print_fail "$(_t "未知 locale: $target （支持: zh / en / reset）" "Unknown locale: $target (supported: zh / en / reset)")"
            return 1
            ;;
    esac
    print_info "$(_t "下次任意运行 yunzes-node 都会按新偏好显示；用 YUNZES_LANG=zh|en 可一次性覆盖" \
                    "All future yunzes-node runs will use the new preference; use YUNZES_LANG=zh|en for one-shot override")"
}

cmd_setup_entry() {
    if ! is_root; then
        print_fail "$(_t "需 root 才能写 $INSTALLED_PATH" "Need root to write $INSTALLED_PATH")"
        return 1
    fi
    if [[ ! -f "$SCRIPT_PATH" ]]; then
        print_fail "$(_t "找不到当前脚本路径: $SCRIPT_PATH" "Script path not found: $SCRIPT_PATH")"
        return 1
    fi
    print_cmd "install -m 0755 $SCRIPT_PATH $INSTALLED_PATH"
    install -m 0755 "$SCRIPT_PATH" "$INSTALLED_PATH"
    print_ok "$(_t "命令已安装 / 更新: $INSTALLED_PATH" "Command installed / updated: $INSTALLED_PATH")"
    print_info "$(_t '现在直接 yunzes-node 即可进入菜单' 'Now `yunzes-node` enters the menu')"
}

# ============================================================================
# M24 dispatch: usage + main argument parser. Subcommands that mutate state
# enforce root via the case below; read-only subcommands (status/logs/verify/
# show-config/lang) are root-optional so non-root operators in the docker
# group can still use them.
# ============================================================================
usage() {
    print_title "yunzes-node v${SCRIPT_VERSION}  —  $(_t '单容器双核心 Docker 部署' 'Single-container dual-core Docker deployment')"
    print_separator
    cat <<EOF

$(_t '用法' 'Usage'):
    yunzes-node                       # $(_t '进入交互菜单' 'enter interactive menu')
    yunzes-node menu                  # $(_t '同上' 'same as above')
    yunzes-node install [--no-restart]
    yunzes-node update                # $(_t '同 upgrade' 'alias of upgrade')
    yunzes-node upgrade
    yunzes-node start | stop | restart
    yunzes-node redeploy [--no-restart]
    yunzes-node status
    yunzes-node logs                  # $(_t '最近 100 行' 'last 100 lines')
    yunzes-node follow-log
    yunzes-node verify                # $(_t '三级验证' '3-tier verify')
    yunzes-node validate-config       # $(_t 'JSON + 业务字段体检' 'JSON + semantic validation')
    yunzes-node edit-config
    yunzes-node show-config           # $(_t '自动隐藏 ApiKey' 'ApiKey masked automatically')
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
    yunzes-node setup-entry           # $(_t "安装到 ${INSTALLED_PATH}" "install to ${INSTALLED_PATH}")
    yunzes-node lang [zh|en|reset]    # $(_t "查看或持久化语言偏好（不传参 = 仅查看）" "show / persist language preference (no arg = show)")

$(_t '环境变量' 'Environment variables'):
    NO_COLOR=1                      $(_t '关闭彩色输出 (适合日志采集 / CI)' 'disable colors (logs / CI)')
    YUNZES_LANG=zh|en               $(_t '一次性强制语言（覆盖持久偏好）' 'one-shot language override (beats persisted preference)')

$(_t '语言' 'Locale'):
    $(_t '默认中文。' 'Default is Chinese.') $(_t '持久切换：' 'Persist switch:') yunzes-node lang en
    $(_t '一次性切换：' 'One-shot:') YUNZES_LANG=en yunzes-node menu
    $(_t '查看当前：' 'Show current:') yunzes-node lang
    $(_t '配置文件：' 'State file:') /opt/yunzes-node/state/locale

$(_t '路径约定' 'Paths'):
    $(_t '配置' 'config')   ${CONFIG_FILE}
    $(_t '证书' 'certs')   ${CERTS_DIR}
    fake               ${FAKE_CERTS_DIR}
    $(_t '备份' 'backup')  ${BACKUP_DIR}
    $(_t '源码' 'source')  ${SRC_DIR}
    $(_t '日志' 'logs')    docker logs ${NAME}

$(_t '详见 README.md。' 'See README.md for details.')
EOF
}

main() {
    SOURCE_DIR="$(detect_source_dir 2>/dev/null || true)"

    local cmd="${1:-menu}"
    [[ $# -gt 0 ]] && shift

    case "$cmd" in
        install|update|upgrade|redeploy|edit-config|gen-config|backup|rollback|fake-test|uninstall|uninstall-full|setup-entry)
            if ! is_root; then
                print_fail "$(_t "请使用 root 运行: sudo $SCRIPT_PATH $cmd $*" "Run as root: sudo $SCRIPT_PATH $cmd $*")"
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
        validate-config) validate_config ;;
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
        lang)            cmd_lang "${1:-}" ;;
        precheck)        precheck "$@" ;;
        version|-v|--version) print_info "yunzes-node script v${SCRIPT_VERSION} ($(_t 'locale' 'locale')=${LOCALE})" ;;
        help|-h|--help)  usage ;;
        *)               usage; exit 1 ;;
    esac
}

main "$@"
