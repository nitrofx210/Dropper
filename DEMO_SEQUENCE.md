# 5-Minute Demo Sequence

**Target**: Faculty code review at 2 PM today  
**Branch**: fix/peer-discovery  
**Repo**: https://github.com/nitrofx210/Dropper

---

## Demo Overview (5 minutes)

| Time | Segment | Focus |
|------|---------|-------|
| 0:00-0:30 | Architecture Overview | Show Mermaid diagram, explain components |
| 0:30-1:30 | Observer Live | Start observer, show peer connections, TX observations |
| 1:30-3:00 | Resolver Demo | Run block resolver, show inclusion results |
| 3:00-4:00 | Analyzer Demo | Run longitudinal analysis, show output |
| 4:00-5:00 | Database Inspection | Use dbinspect tool to show schema & data |

---

## Pre-Demo Setup (Run Before Review)

```bash
# 1. Build all binaries
go build ./cmd/...

# 2. Start local testnet (4-node geth cluster)
docker compose up -d
# Wait ~30s for nodes to sync

# 3. Verify testnet is running
curl -s http://localhost:8545 -X POST -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
```

---

## Segment 1: Architecture Overview (30s)

**Show**: `docs/architecture.mmd` in browser (GitHub renders Mermaid) or VS Code preview

**Key Points to Cover**:
- **Observer**: Native Go binary, connects to mainnet via devp2p/RLPx, observes TX gossip (0x08 announcements, 0x02 broadcasts, 0x0A pooled responses)
- **Storage**: SQLite WAL mode, single-writer pattern, 5 tables (collection_runs, peer_events, transaction_observations, tracked_transactions, transaction_inclusion)
- **Resolver**: Two modes - per-transaction (exponential backoff) and per-block (finalized tag, binary search)
- **Analyzer**: Longitudinal studies, network impact, usefulness scoring
- **Simulation**: Eclipse attack modeling, simnet frameworks
- **External RPC**: DRPC (finalized tag support), Alchemy, Infura, Etherscan
- **Local Testnet**: 4-node Docker compose for validation

---

## Segment 2: Observer Live (60s)

```bash
# Terminal 1: Start observer (passive mode, local testnet)
./ethereum-dropper-observer \
  --config configs/pilot_15min.json \
  --mode passive \
  --label LOCAL_TESTNET \
  --rpc-endpoint http://localhost:8545
```

**What to Show**:
1. Log output: "Dialing bootnodes...", "Connected to peer", "Handshake OK (eth/72)"
2. Peer events table filling: `sqlite3 observer.db "SELECT * FROM peer_events;"`
3. Transaction observations: `sqlite3 observer.db "SELECT tx_hash, message_type, peer_id FROM transaction_observations LIMIT 10;"`
4. Explain: passive mode = honest empty GetPooledTransactions replies, no fetch

**Key Metrics**:
- `observed` counter: TXs decoded from peer messages
- `stored` counter: TXs written to SQLite
- `dropped` counter: Queue full / backpressure

---

## Segment 3: Block Resolver Demo (90s)

```bash
# Terminal 2: Run block resolver (uses finalized tag for reorg safety)
./ethereum-dropper-blockresolver \
  --db observer.db \
  --rpc-endpoint http://localhost:8545 \
  --use-finalized-tag \
  --batch-size 20 \
  --checkpoint-interval 10
```

**What to Show**:
1. Log: "Fetching finalized head...", "Processing block 12345...", "Matched 47 TXs"
2. Checkpoint file: `cat resolver_checkpoint.json` - shows resumability
3. Results in `transaction_inclusion` table:
   ```sql
   SELECT status, COUNT(*) FROM transaction_inclusion GROUP BY status;
   -- not_found_yet | included | not_included_within_window
   ```
4. Explain design:
   - Fetches blocks by number (batch eth_getBlockByNumber)
   - Uses "finalized" block tag (DRPC supports) for reorg safety
   - Binary search on timestamps for TX inclusion window
   - Checkpoint every N blocks for crash resumability
   - Endpoint failover on persistent errors

---

## Segment 4: Analyzer Demo (60s)

```bash
# Run longitudinal analysis
./ethereum-dropper-analyzer \
  --db observer.db \
  --mode longitudinal \
  --output analysis_longitudinal.csv

# Run network impact analysis
./ethereum-dropper-analyzer \
  --db observer.db \
  --mode network-impact \
  --output analysis_network.csv
```

**What to Show**:
1. CSV output columns: `run_id, tx_hash, first_seen_ns, inclusion_delay_ms, hop_count, useful_score`
2. Longitudinal: Compare multiple runs (if available)
3. Network impact: Propagation patterns, peer degree vs inclusion speed
4. Usefulness scoring: Gas price, inclusion speed, announcement-to-broadcast ratio

---

## Segment 5: Database Inspection (60s)

```bash
# Interactive inspector
./ethereum-dropper-dbinspect --db observer.db

# Or quick schema view
sqlite3 observer.db ".schema"
sqlite3 observer.db "SELECT * FROM collection_runs;"
sqlite3 observer.db "SELECT COUNT(*) FROM transaction_observations;"
sqlite3 observer.db "SELECT status, COUNT(*) FROM transaction_inclusion GROUP BY status;"
```

**What to Show**:
1. Schema with WAL mode, indexes
2. Collection run metadata (observer_id, geth_lib_version, config_hash, counters)
3. Peer events with client names/versions (Geth, Erigon, Nethermind, etc.)
4. Transaction observations with message types, source confidence
5. Transaction inclusion with lookup source (rpc_block, etherscan), delay calculations

---

## Backup Plan (If Live Demo Fails)

1. **Pre-recorded output**: `docs/sample/observer_sample.db` - use with dbinspect
2. **Screenshots**: Have terminal screenshots ready in presentation
3. **Explain the code**: Walk through `internal/observer/observer.go`, `internal/resolver/block_resolver.go`

---

## Key Talking Points for Faculty

| Topic | Evidence in Code |
|-------|------------------|
| **Research integrity** | Data labels (OBSERVED/DERIVED/EXTERNAL/INFERRED), provenance in schema |
| **Reproducibility** | Config hashes, git commit, schema version, go version in collection_runs |
| **Performance** | Single-writer SQLite, batched WAL, bounded channels, SetMaxOpenConns(1) |
| **Safety** | Finalized tag, checkpoint resumability, endpoint failover, passive mode by default |
| **Ethics** | No spam, no malformed messages, no eclipse/evasion logic, honest protocol behavior |

---

## Files to Highlight During Review

| File | Purpose |
|------|---------|
| `internal/observer/observer.go` | Main observer loop, peer event subscription, TX handling |
| `internal/observer/ethhandler/handler.go` | ETH protocol handler, message decoding |
| `internal/resolver/block_resolver.go` | Per-block resolver with finalized tag, checkpoint |
| `internal/storage/sqlite.go` | Single-writer pattern, schema, migrations |
| `internal/config/config.go` | Typed config with validation, defaults |
| `docs/ARCHITECTURE.md` | Detailed architecture documentation |
| `docs/CODE_TOUR.md` | Code walkthrough guide |
| `docs/KNOWN_ISSUES.md` | Honest assessment of limitations |