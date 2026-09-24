#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

DOCKER="/c/Program Files/Docker/Docker/resources/bin/docker.exe"

echo "Stopping Ethereum Dropper testbed..."
"$DOCKER" compose down
echo "Testbed stopped."