#!/usr/bin/env bash
# yunzes-node container launcher.
#
# Default mode is host network — the recommended production path. Use
# `--bridge` for a port-mapped fallback suitable for testing, but be aware
# that:
#   - bridge cannot serve ACME HTTP-01 unless host:80 is mapped
#   - bridge UDP requires explicit /udp port mappings
#   - bridge masks the real client IP unless proxy-protocol is configured
#
# This script never installs a config; the operator must place a valid
# /etc/yunzes-node/config.json before launch.

set -euo pipefail

IMAGE=${IMAGE:-yunzes-node:test}
NAME=${NAME:-yunzes-node}
HOST_DIR=${HOST_DIR:-/etc/yunzes-node}
MODE=host
BRIDGE_PORTS=()

usage() {
    cat <<EOF
Usage: $(basename "$0") [--host|--bridge] [--image IMAGE] [--name NAME] [--port SPEC]...

  --host           network_mode=host  (default; Linux only)
  --bridge         bridge network with -p mappings; pass each spec via --port
  --image IMAGE    container image tag       (default: $IMAGE)
  --name  NAME     container name            (default: $NAME)
  --port  SPEC     repeated. e.g. --port 443:443/tcp --port 443:443/udp --port 80:80/tcp
                   (only used with --bridge)
  -h, --help       show this help

Environment overrides: IMAGE, NAME, HOST_DIR.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --host)    MODE=host;   shift ;;
        --bridge)  MODE=bridge; shift ;;
        --image)   IMAGE=$2;    shift 2 ;;
        --name)    NAME=$2;     shift 2 ;;
        --port)    BRIDGE_PORTS+=("$2"); shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *)         echo "unknown flag: $1" >&2; usage; exit 1 ;;
    esac
done

# Pre-create host directories so the bind-mount lands on a real filesystem
# tree. /etc/yunzes-node/certs persists ACME state across container rebuilds.
mkdir -p "$HOST_DIR/certs"

if [[ ! -f "$HOST_DIR/config.json" ]]; then
    echo "[warn] $HOST_DIR/config.json does not exist; the container will exit on startup." >&2
    echo "[warn] Place a valid config.json before re-running this script." >&2
fi

# Replace any prior container so the start is idempotent.
if docker ps -a --format '{{.Names}}' | grep -Fxq "$NAME"; then
    docker rm -f "$NAME" >/dev/null
fi

if [[ "$MODE" == host ]]; then
    docker run -d \
        --name "$NAME" \
        --network host \
        --restart always \
        -v "$HOST_DIR:/etc/yunzes-node" \
        "$IMAGE"
else
    if [[ ${#BRIDGE_PORTS[@]} -eq 0 ]]; then
        # Sensible defaults for a bridge fallback. Operators with non-default
        # ports or UDP protocols (hysteria2/tuic) MUST pass --port explicitly.
        BRIDGE_PORTS=(80:80/tcp 443:443/tcp 443:443/udp)
        echo "[info] no --port given; defaulting to: ${BRIDGE_PORTS[*]}" >&2
        echo "[info] bridge does not satisfy production guidance — see README.md." >&2
    fi
    PORT_FLAGS=()
    for spec in "${BRIDGE_PORTS[@]}"; do
        PORT_FLAGS+=(-p "$spec")
    done
    docker run -d \
        --name "$NAME" \
        --restart always \
        "${PORT_FLAGS[@]}" \
        -v "$HOST_DIR:/etc/yunzes-node" \
        "$IMAGE"
fi

cat <<EOF

yunzes-node container started.

Common follow-up commands:
  docker logs -f $NAME       # tail structured logs (logical_tag, core, runtime_key, ...)
  docker restart $NAME       # restart in place
  docker rm -f $NAME         # stop and remove
  docker ps --filter name=$NAME

If you change /etc/yunzes-node/config.json the container's --watch loop
will reload automatically; otherwise run docker restart.
EOF
