# Ethereum Dropper Lab

A reproducible research testbed for Ethereum P2P networking experiments. This project provides tools to observe Ethereum transaction gossip on mainnet, resolve transaction inclusion, and analyze the results.

## Components

| Command | Description |
|---------|-------------|
| `ethereum-dropper-observer` | Connects to Ethereum mainnet via devp2p/RLPx, observes transaction gossip (announcements, broadcasts, pooled transactions) |
| `ethereum-dropper-resolver` | Resolves transaction inclusion by polling `eth_getTransactionByHash` with exponential backoff |
| `ethereum-dropper-blockresolver` | Resolves transaction inclusion by fetching blocks via `eth_getBlockByNumber` (binary search on timestamps) |
| `ethereum-dropper-analyzer` | Analyzes collected data: longitudinal studies and network-level impact analysis |
| `ethereum-dropper-dbinspect` | Read-only SQLite database inspector for observer/resolver databases |
| `ethereum-dropper-summarize` | Generates a human-readable summary of a collection run |

## Quick Start

### Prerequisites

- Go 1.21+
- Access to an Ethereum JSON-RPC endpoint (for resolver/blockresolver)

### Build

```bash
go build -o observer ./cmd/observer
go build -o resolver ./cmd/resolver
go build -o blockresolver ./cmd/blockresolver
go build -o analyzer ./cmd/analyzer
go build -o dbinspect ./cmd/dbinspect
go build -o summarize ./cmd/summarize
```

### Run the Observer

```bash
# Using defaults (mainnet, passive mode, port 30306)
./observer

# With config file
./observer --config configs/mainnet_observer.json

# Override data label and mode
./observer --label "MY_EXPERIMENT" --mode fetch
```

### Run the Resolver

```bash
# Resolve transactions from observer.db using RPC endpoint
./resolver --rpc-endpoint https://eth-mainnet.example.com

# With config file and custom settings
./resolver --config configs/mainnet_observer.json --rpc-endpoint https://eth-mainnet.example.com --max-attempts 10 --base-delay 2
```

### Run the Block Resolver

```bash
# Binary-search blocks by timestamp, fetch via eth_getBlockByNumber
./blockresolver --rpc-endpoint https://eth-mainnet.example.com --post-run-margin 3600 --batch-size 50
```

### Analyze Results

```bash
# Longitudinal (time-series) analysis
./analyzer --db observer.db --longitudinal --output longitudinal.csv

# Network impact analysis (peer spread, timing)
./analyzer --db observer.db --impact --output impact.csv

# Both analyses
./analyzer --db observer.db
```

### Inspect Database

```bash
# Full summary of latest run
./dbinspect --db observer.db

# Schema only
./dbinspect --db observer.db --schema

# Table counts only
./dbinspect --db observer.db --tables

# Specific run
./dbinspect --db observer.db --run 5
```

### Generate Human-Readable Summary

```bash
# Summary of latest run
./summarize --db observer.db

# Specific run
./summarize --db observer.db --run 5

# Output to file
./summarize --db observer.db --output summary.txt
```

## Configuration

The observer uses a JSON configuration file. All fields are optional; defaults are provided by `MainnetConfig()`.

### Network Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `":30306"` | Local address for P2P listener (non-standard to avoid stale process conflicts) |
| `max_peers` | int | `50` | Maximum number of concurrent peers |
| `bootnodes` | []string | Mainnet bootnodes | ENR/ENODE URLs for initial peer discovery |
| `static_nodes` | []string | `[]` | Static peer ENODEs (bypasses discovery) |
| `nat` | string | `""` | NAT traversal mode: `"any"`, `"extip:<IP>"`, `"none"` |
| `no_discovery` | bool | `false` | Disable peer discovery (use only static nodes) |
| `dial_timeout` | duration | `5s` | Timeout for outbound dial attempts |
| `dial_ratio` | int | `2` | Dial every N peers (1 = dial all, 2 = 50% inbound) |

### Collection Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `"passive"` | Collection mode: `"passive"` (listen only) or `"fetch"` (request pooled transactions) |
| `data_label` | string | `"MAINNET_OBSERVATION"` | Provenance label stored in `collection_runs` |
| `storage_path` | string | `"observer.db"` | SQLite database file path |
| `tx_dissemination_enabled` | bool | `false` | Enable transaction dissemination (relay to peers) |
| `tx_dissemination_rate_limit` | int | `10` | Max transactions relayed per second |

### Run Metadata (stored in `collection_runs`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `observer_id` | string | `"observer-001"` | Unique identifier for this observer instance |
| `geth_lib_version` | string | `"go-ethereum/v1.17.5"` | go-ethereum library version |
| `go_build_version` | string | `"go1.26.5"` | Go compiler version |
| `collector_git_commit` | string | `""` | Git commit hash of the collector build |
| `config_hash` | string | `""` | Hash of the configuration used |
| `schema_version` | string | `"1"` | Database schema version |
| `notes` | string | `""` | Free-form notes |

### Resolver Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rpc_endpoint` | string | `""` | JSON-RPC endpoint for `eth_getTransactionByHash` / `eth_getBlockByNumber` |
| `max_attempts` | int | `5` | Maximum attempts per transaction |
| `base_delay` | duration | `1s` | Base delay for exponential backoff |
| `status_rpc_endpoint` | string | `""` | RPC endpoint for `eth_getBlockByNumber` (ForkID handshake) |
| `status_rpc_timeout` | duration | `10s` | Timeout for status RPC calls |
| `post_run_margin` | int | `3600` | Seconds to wait after observation end before resolving |
| `batch_size` | int | `20` | Blocks per batch RPC request (blockresolver) |
| `max_concurrent_requests` | int | `5` | Max concurrent RPC requests (blockresolver) |

### Logging

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `log_level` | string | `"info"` | Log level: `trace`, `debug`, `info`, `warn`, `error`, `crit` |

## Command-Line Flags

### Observer (`ethereum-dropper-observer`)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config` | string | `""` | Path to configuration file |
| `--label` | string | `"MAINNET_OBSERVATION"` | Data label for provenance |
| `--mode` | string | `"passive"` | Collection mode: passive or fetch |

### Resolver (`ethereum-dropper-resolver`)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config` | string | `""` | Path to configuration file |
| `--label` | string | `"MAINNET_OBSERVATION"` | Data label for provenance |
| `--mode` | string | `"passive"` | Collection mode: passive or fetch |
| `--rpc-endpoint` | string | `""` | JSON-RPC endpoint for eth_getTransactionByHash |
| `--max-attempts` | int | `5` | Maximum attempts to check a transaction |
| `--base-delay` | int | `1` | Base delay in seconds for exponential backoff |

### Block Resolver (`ethereum-dropper-blockresolver`)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config` | string | `""` | Path to configuration file |
| `--label` | string | `"MAINNET_OBSERVATION"` | Data label for provenance |
| `--mode` | string | `"passive"` | Collection mode: passive or fetch |
| `--rpc-endpoint` | string | `""` | JSON-RPC endpoint for eth_getBlockByNumber |
| `--post-run-margin` | int | `3600` | Post-run margin in seconds |
| `--batch-size` | int | `20` | Number of blocks per RPC batch |
| `--max-concurrent-requests` | int | `5` | Maximum concurrent block fetching requests |

### Analyzer (`ethereum-dropper-analyzer`)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config` | string | `""` | Path to configuration file |
| `--db` | string | `""` | Path to SQLite database (overrides config) |
| `--run` | string | `""` | Collection run ID (default: latest) |
| `--output` | string | `""` | Output file (default: stdout) |
| `--longitudinal` | bool | `false` | Run longitudinal study analysis |
| `--impact` | bool | `false` | Run network-level impact analysis |

### DB Inspector (`ethereum-dropper-dbinspect`)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--db` | string | *required* | Path to SQLite database file |
| `--run` | string | `""` | Collection run ID (default: latest) |
| `--tables` | bool | `false` | List tables and row counts only |
| `--schema` | bool | `false` | Print CREATE TABLE schema |
| `--output` | string | `""` | Output file (default: stdout) |

### Summarize (`ethereum-dropper-summarize`)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--db` | string | *required* | Path to SQLite database file |
| `--run` | string | `""` | Collection run ID (default: latest) |
| `--output` | string | `""` | Output file (default: stdout) |

## Database Schema

The observer uses SQLite with WAL mode. Tables:

- `collection_runs` — Metadata for each collection run (git commit, config hash, schema version, mode, data label, timestamps)
- `peer_events` — Peer connection/disconnection/handshake events with timestamps
- `transaction_observations` — Raw transaction gossip observations (tx_hash, peer_id, message_type, timestamp, message size, etc.)
- `tracked_transactions` — Deduplicated transactions with inclusion status (NULL=unknown, 0=not included, 1=included)
- `transaction_inclusion` — Append-only inclusion results keyed by (tx_hash, resolve_run_id)
- `resolver_runs` — Metadata for each resolver run

## Go Commands

| Command | Description |
|---------|-------------|
| `go build ./...` | Build all packages |
| `go test -short ./...` | Run unit tests (skips e2e/network tests) |
| `go vet ./internal/... ./cmd/...` | Static analysis |
| `gofmt -l .` | List files needing formatting |
| `gofmt -w .` | Format all files |

## License

MIT