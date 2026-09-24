# Ethereum Dropper Lab

A reproducible research testbed for Ethereum P2P networking experiments.

## Phase 1: Basic Docker Network

This phase sets up four lightweight Alpine containers connected via a private Docker network.

### Prerequisites

- Docker Engine
- Docker Compose

### Setup

```bash
# Clone repository (if not already done)
git clone <repository-url> ethereum-dropper-lab
cd ethereum-dropper-lab

# Start the network
./scripts/start.sh
```

### Verification

After starting, you should see four containers running:

```bash
docker compose ps
```

Each container will have a static IP in the 172.30.0.0/24 subnet:

- node1: 172.30.0.11
- node2: 172.30.0.12
- node3: 172.30.0.13
- node4: 172.30.0.14

To test connectivity from node1:

```bash
docker compose exec node1 ping -c 3 node2
docker compose exec node1 ping -c 3 node3
docker compose exec node1 ping -c 3 node4
```

You can also inspect network configuration inside a container:

```bash
docker compose exec node1 ip addr
docker compose exec node1 ip route
```

### Management Scripts

- `./scripts/start.sh` - Start the testbed
- `./scripts/stop.sh` - Stop the testbed
- `./scripts/reset.sh` - Stop and remove containers, networks, and volumes

### Next Steps

After verifying basic connectivity, proceed to Phase 2 to replace Alpine containers with Ethereum execution clients.

---

## Phase 2: Private Ethereum Network with Peer Connections

This phase replaces the Alpine containers with Ethereum execution clients (Geth v1.17.5) to create a private Ethereum development network.

### Prerequisites

- Docker Engine
- Docker Compose
- Completed Phase 1

### Genesis Configuration

The network uses a custom genesis file (`configs/genesis/genesis.json`) with:
- Chain ID: 1337
- Difficulty: 0x20000
- Gas Limit: 0x2fefd8
- Pre-funded accounts:
  - 0x0000000000000000000000000000000000000000 (large balance)
  - 0xAbac9F99A31345dB7f388e6873E1CAFC25A2B8dc
  - 0xC210ab0eCD61F2566365b9DfaC53AF2d47373c90
  - 0xc5B66bbD749280E03060E83c95c1bbCC5A6205F1
  - 0xe1E2530C046Fecc728A4d36905A5a7ff41F936E7

### Setup

```bash
# Start the Ethereum network
./scripts/start.sh

# Verify peer connections (run after nodes are fully started)
./scripts/healthcheck.sh
```

### Verification

After starting, you should see four Geth containers running:

```bash
docker compose ps
```

Each container exposes:
- P2P port: 30303
- RPC port: 8545
- WS port: 8546
- IPC: /data/geth.ipc (inside container)

To check peer connections from any node:

```bash
# Check peer count
docker compose exec node1 geth attach --exec "net.peerCount" ipc:/data/geth.ipc

# Check detailed peers list
docker compose exec node1 geth attach --exec "admin.peers" ipc:/data/geth.ipc

# Run healthcheck for comprehensive metrics
./scripts/healthcheck.sh
```

### Management Scripts

- `./scripts/start.sh` - Start the Ethereum testbed
- `./scripts/stop.sh` - Stop the Ethereum testbed
- `./scripts/reset.sh` - Stop and remove containers, networks, and volumes
- `./scripts/healthcheck.sh` - Collect peer metrics and store in results/

### Peer Connection Status (as of last healthcheck)

Based on the latest healthcheck run:
- node1: 3 peers (connected to node2, node3, node4)
- node2: 3 peers (connected to node1, node3, node4)
- node3: Connected to node1 and node2
- node4: Connected to node1 and node2

All nodes are successfully interconnected forming a partial mesh network.

### Next Steps

After verifying peer connectivity, proceed to Phase 3 to test transaction propagation between nodes.

---