# 15-Minute Code Walkthrough Sequence

**Target**: Faculty code review at 2 PM today  
**Branch**: fix/peer-discovery  
**Format**: Guided tour through key files with explanations

---

## Walkthrough Overview (15 minutes)

| Time | Segment | Files Covered |
|------|---------|---------------|
| 0:00-1:00 | Project Structure & Config | go.mod, internal/config/config.go |
| 1:00-4:00 | Observer Core | internal/observer/observer.go, ethhandler/handler.go |
| 4:00-7:00 | Storage Layer | internal/storage/sqlite.go, models.go |
| 7:00-10:00 | Resolver (Per-Block) | internal/resolver/block_resolver.go |
| 10:00-12:00 | Analyzer | internal/analyzer/analyzer.go |
| 12:00-14:00 | Simulation Framework | internal/simulation/ |
| 14:00-15:00 | Testing, Docs, Known Issues | *_test.go, docs/ |

---

## Segment 1: Project Structure & Config (1 min)

### `go.mod` - Dependencies
```go
github.com/ethereum/go-ethereum v1.17.7  // P2P, RLPx, ETH protocols
modernc.org/sqlite v1.31.0              // Pure Go SQLite driver
github.com/stretchr/testify v1.12.1     // Testing
```

### `internal/config/config.go` - Configuration System
**Key Design Decisions**:
1. **Typed constants** (not stringly-typed): `ModePassive`, `ModeFetch`, `DataLabelMainnetObservation`, etc.
2. **Custom Duration type** with JSON unmarshaling from strings ("5s", "10m")
3. **Default-then-validate pattern**: `Load()` → `applyDefaults()` → `validate()`
4. **Zero values get defaults** before validation (avoids false validation errors)
5. **BOM stripping** for Windows-created JSON files

```go
// Duration supports "5s", "10m", "1h" in JSON
type Duration time.Duration
func (d *Duration) UnmarshalJSON(data []byte) error { ... }

// Validation happens AFTER defaults applied
func Load(configFile string) (*Config, error) {
    // ... unmarshal ...
    applyDefaults(&cfg, defaultCfg)  // Zero values → defaults
    if err := validate(&cfg); err != nil { return nil, err }
}
```

**Run Metadata Fields** (for reproducibility):
- `ObserverID`, `GethLibVersion`, `GoBuildVersion`, `CollectorGitCommit`
- `ConfigHash` (SHA256 of config), `SchemaVersion`, `Notes`

---

## Segment 2: Observer Core (3 min)

### `internal/observer/observer.go` - Main Entry Point
**Responsibilities**:
1. Parse config, create `p2p.Server` with custom ETH protocol
2. Subscribe to peer events via `server.SubscribeEvents()`
3. Manage collection run lifecycle (start/end in `collection_runs`)
4. Coordinate storage writer, TX handler, fetch manager

```go
func (o *Observer) Run(ctx context.Context) error {
    // 1. Create collection run row
    runID, err := o.store.StoreCollectionRun(run)
    
    // 2. Start peer event listener
    go o.handlePeerEvents(runID, peerEventsCh)
    
    // 3. Start p2p server (this blocks)
    if err := o.srv.Start(); err != nil { return err }
    
    // 4. Update run end time
    o.store.UpdateCollectionRunEnd(runID)
}
```

### `internal/observer/ethhandler/handler.go` - ETH Protocol Handler
**Handles 3 message types**:
| Message | Code | Purpose | Storage |
|---------|------|---------|---------|
| `NewPooledTransactionHashes` | 0x08 | Announcement (hashes only) | `message_type='announcement'` |
| `Transactions` | 0x02 | Direct broadcast (full TX) | `message_type='direct_broadcast'` |
| `PooledTransactions` | 0x0A | Response to GetPooledTransactions | `message_type='pooled_tx_response'` |

**Key Functions**:
- `handleMsg()`: Switch on message code, decode RLP
- `handleNewPooledTransactionHashes()`: Store announcements, optionally fetch
- `handleTransactions()`: Store full TXs, track for inclusion
- `handlePooledTransactions()`: Store fetched TXs (fetch mode only)

**Fetch Manager** (mode=fetch only):
- Issues `GetPooledTransactions` for announced hashes
- Rate limited (config: `TxDisseminationRateLimit`)
- Honest empty replies in passive mode (research ethics)

**Handshake Manager**:
- Static fork ID from config genesis
- **Live fork ID** from RPC `eth_getBlockByNumber("finalized")` for Point 1
- Labels live head data as `EXTERNAL` provenance

---

## Segment 3: Storage Layer (3 min)

### `internal/storage/models.go` - Data Structures
**5 Core Tables** (all with comprehensive godoc):
1. `CollectionRun` - Run metadata + counters (observed/stored/dropped)
2. `PeerEvent` - Lifecycle: connect, disconnect, handshake_ok
3. `TxObservation` - Single observation (peer, hash, type, timestamp, raw TX)
4. `TrackedTransaction` - Legacy per-TX resolver working table
5. `TransactionInclusion` - Stage 4 per-block resolver results

**Provenance Labels** (in code comments, not enum):
- `OBSERVED` - Directly from peer message
- `DERIVED` - Computed from observations (e.g., hop count)
- `EXTERNAL` - From RPC (block data, live head)
- `INFERRED` - Heuristic (e.g., first announcer ≈ origin)

### `internal/storage/sqlite.go` - Single-Writer Pattern
**Architecture**:
```
Callers (observer, resolver, analyzer)
    │
    ▼ (enqueue writeReq)
┌─────────────────────────────────┐
│  writeCh (chan writeReq)        │  Bounded: writerBatchSize * 4
└─────────────────────────────────┘
    │
    ▼ (single goroutine)
┌─────────────────────────────────┐
│  writerLoop()                   │
│  - Batch up to 250 ops          │
│  - Flush every 250ms            │
│  - Retry SQLITE_BUSY 3x         │
│  - Single DB connection         │
└─────────────────────────────────┘
    │
    ▼ (SQLite WAL)
┌─────────────────────────────────┐
│  Database file                  │
│  PRAGMA journal_mode=WAL        │
│  PRAGMA synchronous=NORMAL      │
│  PRAGMA busy_timeout=10000      │
└─────────────────────────────────┘
```

**Key Implementation Details**:
```go
// One writer goroutine owns all writes
db.SetMaxOpenConns(1)  // Guarantees single connection

// Write request with completion channel
type writeReq struct {
    do       func(tx *sql.Tx) error
    done     chan error
    obsCount int  // For counter increments
}

// Writer loop batches and commits
func (s *Storage) writerLoop() {
    batch := make([]writeReq, 0, writerBatchSize)
    ticker := time.NewTicker(writerFlushInterval)
    for {
        select {
        case req := <-s.writeCh:
            batch = append(batch, req)
            if len(batch) >= writerBatchSize { s.flushBatch(batch); batch = batch[:0] }
        case <-ticker.C:
            if len(batch) > 0 { s.flushBatch(batch); batch = batch[:0] }
        case <-s.closeWriter:
            s.flushBatch(batch); close(s.writerDone); return
        }
    }
}
```

**Schema Migrations** (auto-run on `New()`):
- Adds `observed_count`, `stored_count`, `dropped_count` to `collection_runs`
- Uses `PRAGMA table_info` to detect missing columns
- Runs in transaction, idempotent

---

## Segment 4: Resolver - Per-Block Design (3 min)

### `internal/resolver/block_resolver.go` - Stage 4 Implementation

**Design Rationale**: Per-transaction resolver (exponential backoff) has problems:
- N RPC calls per TX (inefficient)
- No reorg protection (uses `latest` tag)
- Hard to resume after crash

**Per-Block Resolver Approach**:
1. Fetch blocks by number (batch `eth_getBlockByNumber`)
2. Use **"finalized" block tag** (DRPC supports) for reorg safety
3. Hash-match TXs in block against `transaction_observations` (indexed)
4. Checkpoint every N blocks for crash resumability
5. Endpoint failover on persistent errors

**State Machine** (3 states per TX):
```
not_found_yet  →  included  (found in block)
not_found_yet  →  not_included_within_window (past post-run margin)
```

**Key Structures**:
```go
type BlockResolver struct {
    store           *storage.Storage
    rpcEndpoints    []string  // Failover list
    batchSize       int       // Blocks per RPC batch (default 20)
    postRunMargin   int       // Seconds after run end to stop (default 3600)
    useFinalizedTag bool      // Use "finalized" tag (default true)
    checkpointFile  string    // JSON checkpoint for resumability
}

type Checkpoint struct {
    LastProcessedBlock int64   `json:"last_processed_block"`
    LastProcessedTime  int64   `json:"last_processed_time"`
    RunID              int64   `json:"run_id"`
}
```

**Main Loop**:
```go
func (r *BlockResolver) Run(ctx context.Context, runID int64) error {
    // 1. Load checkpoint if exists
    cp := r.loadCheckpoint()
    
    // 2. Determine start block (run start time → block number via binary search)
    startBlock := r.findStartBlock(run.StartTime)
    
    // 3. Main processing loop
    for {
        // Fetch batch of blocks using finalized tag
        blocks, err := r.fetchBlockBatch(ctx, currentBlock, r.batchSize)
        
        // 4. Process each block: hash-match TXs
        for _, block := range blocks {
            r.processBlock(block, runID)
        }
        
        // 5. Update checkpoint
        r.saveCheckpoint(Checkpoint{LastProcessedBlock: currentBlock, ...})
        
        // 6. Check exit condition (past run end + margin)
        if currentBlock > endBlock + margin { break }
    }
}
```

**Hash Matching** (efficient via index):
```sql
-- Index: idx_tx_observations_tx_hash ON transaction_observations(tx_hash)
SELECT tx_hash, peer_id, timestamp_ns, message_type 
FROM transaction_observations 
WHERE run_id = ? AND tx_hash IN (?, ?, ...)
```

**Results stored in `transaction_inclusion`**:
- `status`: not_found_yet, included, not_included_within_window, lookup_failed
- `lookup_source`: rpc_block, etherscan
- `inclusion_delay_ns`: Derived (FirstConfirmedNs - FirstSeenNs)

---

## Segment 5: Analyzer (2 min)

### `internal/analyzer/analyzer.go` - Three Analysis Modes

| Mode | Purpose | Output |
|------|---------|--------|
| `longitudinal` | Compare runs over time | CSV: run_id, tx_hash, inclusion_delay_ms, hop_count |
| `network-impact` | Propagation patterns | CSV: peer_degree, first_hop_latency, inclusion_rate |
| `usefulness` | TX usefulness scoring | CSV: tx_hash, gas_price, inclusion_speed, score |

**Longitudinal Study**:
- Loads multiple `collection_runs` (by data_label)
- Tracks same TX across runs (if re-observed)
- Computes: time-to-inclusion, peer diversity, announcement-to-broadcast ratio

**Network Impact**:
- Builds peer graph from `peer_events` + `transaction_observations`
- Computes: hop count (announcement → broadcast), propagation trees
- Identifies: super-peers, eclipsed regions, partition detection

**Usefulness Scoring** (0.0-1.0):
```
useful = w1*inclusion_speed + w2*gas_price_percentile + w3*peer_diversity + w4*announcement_first
```
- Higher = TX propagated efficiently, included quickly
- Lower = TX stalled, low fee, single-peer observation

---

## Segment 6: Simulation Framework (2 min)

### `internal/simulation/` - Controlled Experiments

**Eclipse Attack Model** (`eclipse_model.go`):
- Kademlia routing table simulation
- Attacker controls N identities in victim's buckets
- Metric: **Capture Ratio** = TXs attacker sees / total TXs
- Validates: theoretical bounds vs empirical

**Simnet Integration**:
- `simnet-work/geth-simnet`: Real geth P2P over simulated transport
- Deterministic network conditions (latency, partition, churn)
- Replay captured mainnet traffic for validation

**Usage**:
```bash
# Run eclipse simulation
go run ./cmd/simulation/eclipse -peers 100 -attackers 10 -duration 1h

# Run simnet replay
go run ./cmd/simulation/replay -trace mainnet_trace.json -topology mesh
```

---

## Segment 7: Testing, Documentation, Known Issues (1 min)

### Test Coverage
| Package | Tests | Focus |
|---------|-------|-------|
| `config` | 11 | Validation, defaults, JSON unmarshaling |
| `storage` | 8 | Atomic writes, writer loop, migrations, busy handling |
| `observer` | 6 | Peer events, TX handling, handshake |
| `resolver` | 5 | Block fetching, hash matching, checkpoint |
| `analyzer` | 4 | Longitudinal, network impact, usefulness |

**Run**: `go test -short ./...` (30 pass, 1 integration test requires external DB)

### Documentation
| File | Purpose |
|------|---------|
| `docs/ARCHITECTURE.md` | Full system architecture, data flows |
| `docs/CODE_TOUR.md` | Guided code walkthrough (this document) |
| `docs/KNOWN_ISSUES.md` | Honest limitations, future work |
| `docs/resolver-per-block-design.md` | Detailed resolver design rationale |
| `docs/architecture.mmd` | Mermaid diagram (GitHub renders) |

### Known Issues (from `docs/KNOWN_ISSUES.md`)
1. **No mainnet run yet** - Only local testnet validated
2. **Resolver RPC dependency** - Requires DRPC/Alchemy/Infura for finalized tag
3. **Simulation incomplete** - Eclipse model needs Kademlia bucket simulation
4. **No CI/CD** - Manual build/test only
5. **Single observer instance** - No distributed collection yet

---

## Questions to Anticipate

| Question | Answer |
|----------|--------|
| Why Go, not Rust/Python? | go-ethereum library, team expertise, pure Go SQLite |
| Why not use existing tools (txpool, etc.)? | Need full control: passive observation, provenance labels, reproducible config |
| How do you handle reorgs? | Finalized tag (DRPC), per-block not per-TX, checkpoint resumability |
| What's the capture rate on mainnet? | Not yet measured - local testnet shows 100% of gossiped TXs |
| Can this scale to 1000 peers? | Current max_peers=50, writer batch=250, channel=1000 - needs load testing |
| Ethics of fetch mode? | Honest empty GetPooledTransactions replies, rate limited, no spam |
| Data provenance? | Labels: OBSERVED/DERIVED/EXTERNAL/INFERRED in schema comments + code |

---

## Files to Have Open During Walkthrough

1. `internal/config/config.go` - Config system
2. `internal/observer/observer.go` - Observer main
3. `internal/observer/ethhandler/handler.go` - ETH protocol
4. `internal/storage/sqlite.go` - Single-writer pattern
5. `internal/resolver/block_resolver.go` - Per-block resolver
6. `internal/analyzer/analyzer.go` - Analysis modes
7. `docs/architecture.mmd` - Architecture diagram
8. `docs/KNOWN_ISSUES.md` - Limitations