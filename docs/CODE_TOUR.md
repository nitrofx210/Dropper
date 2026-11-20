# Code Tour

This document guides a reviewer through the codebase, highlighting entry points, key data flows, and important implementation details.

## Repository Structure

```
ethereum-dropper-lab/
├── cmd/
│   ├── observer/          # P2P observer entry point
│   ├── resolver/          # Transaction inclusion resolver (eth_getTransactionByHash)
│   ├── blockresolver/     # Block-based resolver (eth_getBlockByNumber)
│   ├── analyzer/          # Longitudinal & impact analysis
│   ├── dbinspect/         # Read-only database inspector
│   └── summarize/         # Human-readable run summary
├── internal/
│   ├── config/            # Configuration loading & defaults
│   ├── observer/          # P2P observer core
│   │   └── ethhandler/    # eth/69-72 protocol message handling
│   ├── resolver/          # Resolver implementations
│   └── storage/           # SQLite storage layer
├── configs/               # Example configuration files
├── docs/                  # Documentation
└── scripts/               # Utility scripts
```

## Entry Points

### Observer (`cmd/observer/main.go`)
- Parses `--config`, `--label`, `--mode` flags
- Loads config (file → defaults → flag overrides)
- Creates `observer.New(cfg, configFile)`
- Starts with `obs.Start(ctx)` — blocks until context cancelled

### Resolver (`cmd/resolver/main.go`)
- Parses flags including `--rpc-endpoint`, `--max-attempts`, `--base-delay`
- Opens storage, creates `resolver.NewResolver()`
- Runs `res.Run(ctx)` — processes tracked transactions with exponential backoff

### Block Resolver (`cmd/blockresolver/main.go`)
- Similar to resolver but uses `resolver.NewBlockResolver()`
- Binary searches block timestamps, fetches blocks in batches

### Analyzer (`cmd/analyzer/main.go`)
- Opens database, determines run ID (latest or `--run`)
- Runs `--longitudinal` (hourly buckets) and/or `--impact` (peer spread)

### DBInspect (`cmd/dbinspect/main.go`)
- Opens database **read-only** (`mode=ro` URI)
- Prints schema, table counts, run summary, peer events, observations, tracked transactions, inclusion, resolver runs

### Summarize (`cmd/summarize/main.go`)
- Generates human-readable summary of a collection run

## Core Packages

### `internal/config/config.go`
**Key types:**
- `Config` — All settings (30+ fields) with JSON tags
- `MainnetConfig()` — Hardcoded defaults for mainnet
- `Load(file)` — Loads JSON, merges with defaults

**Important:** Config hash (SHA256 of effective config) is computed at runtime and stored in `collection_runs.config_hash` for provenance.

### `internal/observer/observer.go`
**Key types:**
- `Observer` — Main struct holding P2P server, storage, config, run metadata
- `New(cfg, configFile)` — Initializes storage, creates run record, sets up P2P server
- `Start(ctx)` — Starts P2P server, peer manager, stats logger; blocks until ctx done

**Key methods:**
- `initP2PServer()` — Creates devp2p server with eth protocols (69-72)
- `logStatusHandshake()` — Logs handshake details including ForkID
- `handlePeerEvent()` — Records peer connect/disconnect/handshake to `peer_events`
- `statsLogger()` — Periodic logging of observations, peers, queue depth

**Concurrency:**
- One goroutine per connected peer (read loop → handleMessage)
- Single storage writer goroutine (channel-serialized)
- Stats logger on ticker

### `internal/observer/ethhandler/handler.go`
**Key types:**
- `EthHandler` — Implements `p2p.ProtocolHandler` for eth/69-72
- `ProtocolVersions()` — Returns [69, 70, 71, 72]
- `doHandshake(peer, rw, version)` — Sends/receives Status packet, validates ForkID

**Message dispatch (`handleMessage`):**
| Code | Packet Type | Handler |
|------|-------------|---------|
| 0x01 | Status | `handleStatus` |
| 0x02 | NewBlockHashes | `handleNewBlockHashes` |
| 0x03 | Transactions | `handleTransactions` |
| 0x08 | NewPooledTransactionHashes | `handleNewPooledTransactionHashes` |
| 0x09 | GetPooledTransactions | `handleGetPooledTransactions` |
| 0x0A | PooledTransactions | `handlePooledTransactions` |

**Transaction observation flow:**
1. Receive announcement/broadcast/response
2. Extract tx hashes, types, sizes
3. Call `sto.StoreTxObservations([]BatchTxObservation)`
4. Storage deduplicates into `tracked_transactions`

**Critical: Hash Comparison**
- DB stores raw 32-byte `tx_hash` (BLOB)
- RPC returns hex string with `0x` prefix
- **Bug fixed:** Compare using `hex.EncodeToString(dbHash)` vs `strings.TrimPrefix(rpcHash, "0x")` padded to 64 chars

### `internal/storage/sqlite.go`
**Key types:**
- `Storage` — Wraps `*sql.DB`, write channel, stats counters
- `WriteRequest` — Enqueued write operations

**Schema (6 tables):**
```sql
collection_runs          -- Run metadata, provenance
peer_events              -- Connect/disconnect/handshake events
transaction_observations -- Raw gossip observations (append-only)
tracked_transactions     -- Deduplicated tx with inclusion status
transaction_inclusion    -- Append-only inclusion results (tx_hash, resolve_run_id)
resolver_runs            -- Resolver run metadata
```

**Writer Loop:**
```go
func (s *Storage) writerLoop() {
    for req := range s.writeCh {
        s.batch = append(s.batch, req)
        if len(s.batch) >= batchSize || time.Since(lastCommit) > batchTimeout {
            s.commitBatch()
        }
    }
    // Drain remaining on close
}
```

**Busy Retry:**
- On `SQLITE_BUSY`, retries with exponential backoff (10ms, 20ms, 40ms... up to 1s)
- Max 100 retries before failing

**Key Methods:**
- `StoreTxObservations()` — Batch insert observations, upsert tracked_transactions
- `StorePeerEvent()` — Insert peer event
- `UpdateTrackedTransactionInclusion()` — Update inclusion status
- `StoreInclusionResult()` — Append to transaction_inclusion

### `internal/resolver/resolver.go`
**Two Resolvers:**

1. **Resolver (per-transaction polling)**
   - Fetches tracked transactions with `included IS NULL`
   - Calls `eth_getTransactionByHash` with exponential backoff
   - Updates `tracked_transactions.included` and appends to `transaction_inclusion`

2. **BlockResolver (block-based)**
   - Binary searches block timestamps via `eth_getBlockByNumber(finalized)`
   - Fetches blocks in batches, checks all transactions in block
   - More efficient for large datasets

**Exponential Backoff:**
```
Attempt 1: 1s    (base_delay)
Attempt 2: 2s    (base_delay * 2)
Attempt 3: 4s    (base_delay * 4)
Attempt 4: 8s    (base_delay * 8)
Attempt 5: 16s   (base_delay * 16)
```

### `internal/resolver/block_resolver.go`
**Key Algorithm: Binary Search on Timestamps**
```go
func (r *BlockResolver) findBlockAtTime(targetTime int64) (uint64, error) {
    low := r.firstBlockAfterRunStart
    high := r.currentHeadBlock
    for low <= high {
        mid := (low + high) / 2
        block := r.fetchBlock(mid)
        if block.Timestamp < targetTime {
            low = mid + 1
        } else {
            high = mid - 1
            result = mid
        }
    }
    return result, nil
}
```

## Data Flow: Transaction Observation → Inclusion Resolution

```
1. Observer receives NewPooledTransactionHashes (0x08)
   ├─ Extracts: []Hash, []Type, []Size, []CustodyMask (ETH72)
   └─ Calls sto.StoreTxObservations()

2. Storage (writerLoop)
   ├─ INSERT INTO transaction_observations (run_id, tx_hash, peer_id, ...)
   ├─ UPSERT INTO tracked_transactions (tx_hash, run_id, first_seen, last_seen, count, included=NULL)
   └─ Returns

3. Resolver (later, separate process)
   ├─ SELECT * FROM tracked_transactions WHERE included IS NULL
   ├─ For each: eth_getTransactionByHash (with backoff)
   ├─ If receipt found: included=1, block=receipt.BlockNumber
   ├─ If not found after max_attempts: included=0
   ├─ UPDATE tracked_transactions SET included=?
   └─ INSERT INTO transaction_inclusion (tx_hash, resolve_run_id, ...)

4. BlockResolver (alternative)
   ├─ Binary search blocks covering run time range
   ├─ Fetch blocks in batches via eth_getBlockByNumber
   ├─ For each tx in block: check if in tracked_transactions
   ├─ Record inclusion in transaction_inclusion
   └─ More efficient for high-volume runs
```

## Important Implementation Details

### WAL Mode & Single Writer
- SQLite opened with `_journal_mode=WAL` and `_busy_timeout=5000`
- All writes serialized through single channel → one goroutine
- Readers (analyzer, dbinspect) use separate connections, read-only mode

### Collection Run Provenance
Each run records:
```go
CollectionRun{
    StartTime:          time.Now(),
    ObserverID:         cfg.ObserverID,
    GethLibVersion:     cfg.GethLibVersion,
    GoBuildVersion:     cfg.GoBuildVersion,
    CollectorGitCommit: gitCommit,      // from build ldflags
    ConfigHash:         configHash,     // SHA256 of effective config JSON
    SchemaVersion:      cfg.SchemaVersion,
    Mode:               cfg.Mode,
    DataLabel:          cfg.DataLabel,
    Notes:              cfg.Notes,
}
```

### Dial Ratio
- `dial_ratio = N` means dial 1 out of every N discovered peers
- Default 2 = 50% inbound, 50% outbound
- Ratio 1 = dial all (zero inbound capacity)
- Implemented in `observer.peerManager.dialCandidates()`

### Message Size Tracking
- `AnnouncedSize` from NewPooledTransactionHashes (Sizes field)
- `RawTx` stored for Transactions/PooledTransactions messages
- Enables dissemination analysis (announcement vs full tx size)

### Peer Connection Age
- Recorded at handshake completion: `peerHandshakeNs[peerID] = time.Now().UnixNano()`
- At observation: `conn_age_ms = (now - handshake_ns) / 1e6`
- If no handshake recorded: `NULL`

## Testing

| Test File | Purpose |
|-----------|---------|
| `internal/storage/atomic_test.go` | Atomic counter tests (in-memory DB) |
| `internal/storage/busy_test.go` | Concurrent writers + readers, VACUUM INTO verification |
| `internal/storage/async_test.go` | Async write channel behavior |
| `internal/observer/ethhandler/e2e_test.go` | Full protocol flow over MsgPipe (ETH69-72) |
| `internal/storage/test/storage_test.go` | Storage helper functions |

Run with: `go test -short ./internal/... ./cmd/...`

## Known Quirks

1. **Root-level `verify_*.go` and `check_*.go` files** — One-off analysis scripts with `main` functions; cause `go build ./...` to fail if not excluded. They're in `.gitignore` but still in working tree.

2. **Large database files in repo** — Several `mainnet_*.db` files (100MB–14GB) committed. Should use Git LFS or external storage.

3. **No Go module for test fixtures** — Test databases created on-the-fly in temp files.

4. **Build metadata via ldflags** — `CollectorGitCommit` set at build time; empty if not built with `-ldflags`.