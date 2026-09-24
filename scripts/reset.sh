#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

DOCKER="/c/Program Files/Docker/Docker/resources/bin/docker.exe"

echo "Resetting Ethereum Dropper testbed (stopping and removing containers, network, and volumes)..."
"$DOCKER" compose down -v
echo "Testbed reset."