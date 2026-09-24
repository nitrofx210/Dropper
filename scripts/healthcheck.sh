#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
RESULTS_DIR="$PROJECT_DIR/results"
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
OUTPUT_FILE="$RESULTS_DIR/peer_metrics_$TIMESTAMP.json"

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

# Create temporary file for JSON output
TEMP_FILE=$(mktemp)

# Start JSON array
echo "[" > "$TEMP_FILE"

# Function to get metrics from a node
get_node_metrics() {
    local node_num=$1
    local container_name="ethdropper_node${node_num}"
    local ip_address="172.30.0.1${node_num}"

    echo "Collecting metrics from ${container_name}..."

    # Get enode
    local enode=$(docker compose exec "node${node_num}" geth attach --exec "admin.nodeInfo.enode" ipc:/data/geth.ipc 2>/dev/null | tr -d '"' || echo "null")

    # Get peer count
    local peer_count=$(docker compose exec "node${node_num}" geth attach --exec "net.peerCount" ipc:/data/geth.ipc 2>/dev/null || echo "0x0")

    # Get block number
    local block_number=$(docker compose exec "node${node_num}" geth attach --exec "eth.blockNumber" ipc:/data/geth.ipc 2>/dev/null || echo "0x0")

    # Get detailed peers info
    local peers_info=$(docker compose exec "node${node_num}" geth attach --exec "admin.peers" ipc:/data/geth.ipc 2>/dev/null || echo "[]")

    # Create JSON object for this node
    cat >> "$TEMP_FILE" << EOF
{
  "timestamp": "$(date -u +"%Y-%m-%dT%H:%M:%SZ")",
  "node": "node${node_num}",
  "container_name": "${container_name}",
  "ip_address": "${ip_address}",
  "enode": ${enode},
  "peer_count": "${peer_count}",
  "block_number": "${block_number}",
  "peers_info": ${peers_info}
}
EOF

    # Add comma if not the last node
    if [ "$node_num" -lt 4 ]; then
        echo "," >> "$TEMP_FILE"
    fi
}

# Collect metrics from all 4 nodes
for i in {1..4}; do
    get_node_metrics $i
done

# End JSON array
echo "]" >> "$TEMP_FILE"

# Format JSON with proper indentation (if jq is available)
if command -v jq >/dev/null 2>&1; then
    jq . "$TEMP_FILE" > "$OUTPUT_FILE"
else
    mv "$TEMP_FILE" "$OUTPUT_FILE"
fi

# Clean up temp file
rm -f "$TEMP_FILE"

echo "Peer metrics saved to: $OUTPUT_FILE"

# Also create a latest symlink for easy access
ln -sf "$(basename "$OUTPUT_FILE")" "$RESULTS_DIR/latest_peer_metrics.json" 2>/dev/null || true