#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

echo "Starting Ethereum Dropper testbed..."
docker compose up -d
echo "Testbed started. Use 'docker compose ps' to see running containers."
echo "To verify connectivity, run:"
echo "  docker compose exec node1 ping -c 3 node2"
echo "  docker compose exec node1 ping -c 3 node3"
echo "  docker compose exec node1 ping -c 3 node4"
