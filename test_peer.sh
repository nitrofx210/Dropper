#!/usr/bin/env bash
set -euo pipefail

export MSYS_NO_PATHCONV=1

NODE2_ENODE_RAW=$(docker compose exec node2 geth attach --exec "admin.nodeInfo.enode" ipc:/data/geth.ipc 2>/dev/null | tr -d '\"')
echo "NODE2_ENODE_RAW: $NODE2_ENODE_RAW"

# Try format 1: with enode://
echo "Trying format 1: with enode://"
docker compose exec node1 sh -c "echo \"admin.addPeer('$NODE2_ENODE_RAW')\" | geth attach ipc:/data/geth.ipc" 2>&1 | grep -v "Welcome to the Geth JavaScript console" | grep -v "To exit"

# Try format 2: without enode://
NODE2_ENODE_HEX=$(echo $NODE2_ENODE_RAW | sed 's|enode://||')
echo "Trying format 2: without enode:// - $NODE2_ENODE_HEX"
docker compose exec node1 sh -c "echo \"admin.addPeer('$NODE2_ENODE_HEX')\" | geth attach ipc:/data/geth.ipc" 2>&1 | grep -v "Welcome to the Geth JavaScript console" | grep -v "To exit"

# Try format 3: with enr: prefix (guess)
echo "Trying format 3: with enr: prefix"
docker compose exec node1 sh -c "echo \"admin.addPeer('enr:$NODE2_ENODE_HEX')\" | geth attach ipc:/data/geth.ipc" 2>&1 | grep -v "Welcome to the Geth JavaScript console" | grep -v "To exit"

# Try format 4: just the hex part without @ and port (probably wrong but let's try)
NODE2_IP=$(echo $NODE2_ENODE_HEX | cut -d'@' -f2 | cut -d':' -f1)
NODE2_PORT=$(echo $NODE2_ENODE_HEX | cut -d'@' -f2 | cut -d':' -f2 | cut -d'?' -f1)
NODE2_ID=$(echo $NODE2_ENODE_HEX | cut -d'@' -f1)
echo "Trying format 4: just ID - $NODE2_ID"
docker compose exec node1 sh -c "echo \"admin.addPeer('$NODE2_ID')\" | geth attach ipc:/data/geth.ipc" 2>&1 | grep -v "Welcome to the Geth JavaScript console" | grep -v "To exit"