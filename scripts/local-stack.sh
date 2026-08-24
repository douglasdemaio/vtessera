#!/usr/bin/env bash
# local-stack.sh — one-command Vtessera dev stack
#
# Mirrors the Flatpak GUI's "Start" button from the CLI:
#   - Auto-detects LAN IP for the advertised endpoint
#   - Generates (or reuses) an Ed25519 identity key
#   - Auto-registers the node pubkey in the marketplace key registry
#   - Starts offer-index, vtessera-node, and marketplace-server
#
# Usage:
#   ./scripts/local-stack.sh start   # start all services
#   ./scripts/local-stack.sh stop    # stop all services
#   ./scripts/local-stack.sh status  # check which services are running
#
# Environment overrides:
#   VTESSERA_LOCAL_ONLY=1   bind everything to 127.0.0.1 (no LAN exposure)
#   VTESSERA_MODE=free      "free" (default) or "paid"
#   VTESSERA_PORT=8402      node HTTP port (default 8402)
#   VTESSERA_STATE_DIR=...  state directory (default ~/.local/share/vtessera/stack)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${VTESSERA_MODE:-free}"
LOCAL_ONLY="${VTESSERA_LOCAL_ONLY:-0}"
PORT="${VTESSERA_PORT:-8402}"
INDEX_PORT=8403
MARKETPLACE_PORT=8443
STATE_DIR="${VTESSERA_STATE_DIR:-$HOME/.local/share/vtessera/stack}"
PID_DIR="$STATE_DIR/pids"

# --- colours (no-op if not a tty) ---
if [ -t 1 ]; then
    GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
else
    GREEN=''; RED=''; YELLOW=''; NC=''
fi

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
fail()  { echo -e "${RED}[x]${NC} $*" >&2; exit 1; }

# --- LAN IP detection (mirrors GUI's detect_lan_ip) ---
detect_lan_ip() {
    python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
try:
    s.connect(('8.8.8.8', 80))
    print(s.getsockname()[0])
except Exception:
    print('127.0.0.1')
finally:
    s.close()
" 2>/dev/null || echo "127.0.0.1"
}

# --- hex to base58 (Bitcoin alphabet, matches GUI's base58_encode) ---
hex_to_base58() {
    python3 -c "
import sys
ALPHABET = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'
hex_str = sys.argv[1]
n = int(hex_str, 16)
result = ''
while n > 0:
    n, r = divmod(n, 58)
    result = ALPHABET[r] + result
for c in hex_str:
    if c == '0':
        result = '1' + result
    else:
        break
print(result)
" "$1"
}

# --- ensure key exists ---
ensure_key() {
    local key_path="$STATE_DIR/identity.key"
    if [ -f "$key_path" ]; then
        info "Reusing existing key: $key_path" >&2
    else
        mkdir -p "$STATE_DIR"
        openssl rand 32 > "$key_path" 2>/dev/null
        chmod 600 "$key_path"
        info "Generated new key: $key_path" >&2
    fi
    echo "$key_path"
}

# --- register node in marketplace key registry ---
register_in_marketplace() {
    local pubkey_hex="$1"
    local node_id="$2"
    local keys_path="$STATE_DIR/marketplace/keys.toml"

    mkdir -p "$(dirname "$keys_path")"

    # Skip if already registered
    if [ -f "$keys_path" ] && grep -q "$node_id" "$keys_path" 2>/dev/null; then
        info "Node already registered in key registry"
        return
    fi

    local b58
    b58=$(hex_to_base58 "$pubkey_hex")

    {
        if [ -f "$keys_path" ] && [ -s "$keys_path" ]; then
            cat "$keys_path"
        fi
        echo ""
        echo "[[keys]]"
        echo "name = \"$node_id\""
        echo "pubkey = \"$b58\""
    } > "$keys_path"

    info "Registered node in key registry: $keys_path"
}

# --- generate marketplace server config ---
gen_marketplace_config() {
    local config_path="$STATE_DIR/marketplace/server.toml"
    local keys_path="$STATE_DIR/marketplace/keys.toml"
    local storage_path="$STATE_DIR/marketplace/receipts.jsonl"
    local listen

    if [ "$LOCAL_ONLY" = "1" ]; then
        listen="127.0.0.1:$MARKETPLACE_PORT"
    else
        listen="0.0.0.0:$MARKETPLACE_PORT"
    fi

    mkdir -p "$(dirname "$config_path")"
    cat > "$config_path" <<TOML
listen_addr = "$listen"
key_registry_path = "$keys_path"
storage_path = "$storage_path"
TOML
    info "Marketplace config: $config_path"
}

# --- write PID file ---
save_pid() {
    local name="$1" pid="$2"
    echo "$pid" > "$PID_DIR/$name.pid"
}

# --- check if a service is running ---
is_running() {
    local name="$1"
    local pidfile="$PID_DIR/$name.pid"
    if [ -f "$pidfile" ]; then
        local pid
        pid=$(cat "$pidfile")
        if kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi
    return 1
}

# --- wait for HTTP healthz ---
wait_for() {
    local name="$1" url="$2" tries="${3:-30}"
    for _ in $(seq 1 "$tries"); do
        if curl -sf -m 1 "$url" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
    done
    return 1
}

# =====================================================================
# START
# =====================================================================
cmd_start() {
    mkdir -p "$PID_DIR"

    # Check if already running
    for svc in offer-index node marketplace; do
        if is_running "$svc"; then
            warn "$svc already running (pid $(cat "$PID_DIR/$svc.pid"))"
        fi
    done

    # Detect LAN IP
    local lan_ip
    lan_ip=$(detect_lan_ip)
    local bind_host="0.0.0.0"
    local advertise_host="$lan_ip"
    if [ "$LOCAL_ONLY" = "1" ]; then
        bind_host="127.0.0.1"
        advertise_host="127.0.0.1"
        info "Local-only mode: all binds and endpoints on loopback"
    fi

    # Ensure identity key
    local key_path
    key_path=$(ensure_key)

    # Build binaries first (needed for derive_node_id and gen_offer)
    info "Building binaries..."
    cargo build -q -p vtessera-offer-index --bin vtessera-offer-index --features serve
    cargo build -q -p vtessera-node-api --bin vtessera-node --features serve
    cargo build -q -p marketplace-server --bin marketplace-server

    # Derive node_id and pubkey from the key
    local tmp_offer
    tmp_offer=$(mktemp)
    cargo run -q -p vtessera-node-api --example gen_offer -- \
        free --key "$key_path" --endpoint "http://127.0.0.1:0" \
        > "$tmp_offer" 2>/dev/null
    local node_id pubkey_hex
    node_id=$(python3 -c "import json,sys;print(json.load(sys.stdin)['body']['node_id'])" < "$tmp_offer")
    pubkey_hex=$(python3 -c "import json,sys;print(json.load(sys.stdin)['pubkey_hex'])" < "$tmp_offer")
    rm -f "$tmp_offer"
    info "Node ID: $node_id"

    # Register in marketplace
    register_in_marketplace "$pubkey_hex" "$node_id"

    # Generate marketplace config
    gen_marketplace_config

    # --- offer-index ---
    if ! is_running offer-index; then
        info "Starting offer-index on $bind_host:$INDEX_PORT"
        "$ROOT/target/debug/vtessera-offer-index" \
            --bind "$bind_host:$INDEX_PORT" \
            >/dev/null 2>&1 &
        save_pid offer-index $!
        if wait_for offer-index "http://127.0.0.1:$INDEX_PORT/healthz"; then
            info "offer-index ready"
        else
            fail "offer-index failed to start"
        fi
    fi

    # --- generate offer with actual key ---
    local offer_json="$STATE_DIR/offer.json"
    local endpoint="http://${advertise_host}:${PORT}"
    cargo run -q -p vtessera-node-api --example gen_offer -- \
        "$MODE" --key "$key_path" --endpoint "$endpoint" \
        > "$offer_json" 2>/dev/null
    info "Offer generated for $endpoint ($MODE)"

    # --- vtessera-node ---
    if ! is_running node; then
        local escrow="6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma"
        local network="solana-devnet"
        info "Starting vtessera-node on $bind_host:$PORT"
        "$ROOT/target/debug/vtessera-node" \
            --bind "$bind_host:$PORT" \
            --offer "$offer_json" \
            --key "$key_path" \
            --escrow "$escrow" \
            --network "$network" \
            --backend noop-cpu \
            --state-dir "$STATE_DIR" \
            --publish "http://$advertise_host:$INDEX_PORT" \
            --publish-interval 30 \
            >/dev/null 2>&1 &
        save_pid node $!
        sleep 1
        if wait_for node "http://127.0.0.1:$PORT/healthz"; then
            info "vtessera-node ready"
        else
            warn "vtessera-node may still be starting..."
        fi
    fi

    # Write discovery file for vtessera-agent --local (mirrors GUI behavior)
    local discovery_path="$HOME/.local/share/vtessera/node-discovery.json"
    local node_pid
    node_pid=$(cat "$PID_DIR/node.pid")
    mkdir -p "$(dirname "$discovery_path")"
    cat > "$discovery_path" <<JSON
{
  "endpoint": "http://$advertise_host:$PORT",
  "node_id": "$node_id",
  "index": "http://$advertise_host:$INDEX_PORT",
  "pid": $node_pid
}
JSON
    info "Discovery file: $discovery_path"

    # --- marketplace-server ---
    if ! is_running marketplace; then
        local config_path="$STATE_DIR/marketplace/server.toml"
        if [ -f "$ROOT/target/debug/marketplace-server" ]; then
            info "Starting marketplace-server on $bind_host:$MARKETPLACE_PORT"
            "$ROOT/target/debug/marketplace-server" "$config_path" \
                >/dev/null 2>&1 &
            save_pid marketplace $!
            sleep 1
            info "marketplace-server started"
        else
            warn "marketplace-server binary not found — skipping"
        fi
    fi

    echo
    info "Stack running:"
    info "  offer-index:       http://$advertise_host:$INDEX_PORT"
    info "  vtessera-node:     http://$advertise_host:$PORT"
    info "  marketplace:       http://$advertise_host:$MARKETPLACE_PORT"
    info "  state dir:         $STATE_DIR"
    info "  mode:              $MODE"
    echo
    info "Test with:"
    info "  curl http://127.0.0.1:$PORT/healthz"
    info "  curl http://127.0.0.1:$PORT/offer"
    info "  vtessera-agent --node http://$advertise_host:$PORT health"
}

# =====================================================================
# STOP
# =====================================================================
cmd_stop() {
    local stopped=0
    for svc in marketplace node offer-index; do
        if is_running "$svc"; then
            local pid
            pid=$(cat "$PID_DIR/$svc.pid")
            info "Stopping $svc (pid $pid)"
            kill "$pid" 2>/dev/null || true
            for _ in $(seq 1 10); do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.2
            done
            kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
            rm -f "$PID_DIR/$svc.pid"
            stopped=1
        else
            rm -f "$PID_DIR/$svc.pid"
        fi
    done
    if [ "$stopped" -eq 1 ]; then
        # Remove discovery file (mirrors GUI cleanup)
        rm -f "$HOME/.local/share/vtessera/node-discovery.json"
        info "All services stopped"
    else
        warn "No services were running"
    fi
}

# =====================================================================
# STATUS
# =====================================================================
cmd_status() {
    local all_ok=true
    for svc in offer-index node marketplace; do
        if is_running "$svc"; then
            echo -e "${GREEN}✓${NC} $svc (pid $(cat "$PID_DIR/$svc.pid"))"
        else
            echo -e "${RED}✗${NC} $svc"
            all_ok=false
        fi
    done
    if $all_ok; then
        echo
        info "All services running"
    fi
}

# =====================================================================
# MAIN
# =====================================================================
case "${1:-}" in
    start)  cmd_start ;;
    stop)   cmd_stop ;;
    status) cmd_status ;;
    *)
        echo "Usage: $0 {start|stop|status}"
        echo
        echo "Environment:"
        echo "  VTESSERA_LOCAL_ONLY=1   loopback-only mode (default: LAN-advertised)"
        echo "  VTESSERA_MODE=free      free or paid (default: free)"
        echo "  VTESSERA_PORT=8402      node port (default: 8402)"
        echo "  VTESSERA_STATE_DIR=...  state directory"
        exit 1
        ;;
esac
