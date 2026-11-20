# Known Issues

This document tracks known issues, limitations, and technical debt in the codebase.

## Critical Issues

### 1. Root-Level Test Scripts Break `go build ./...`
**Files:** `verify_original.go`, `verify_quickcheck.go`, `verify_backup.go`, `verify_backup_v2.go`, `verify_last.go`, `check_original_db.go`, `check_original_db2.go`, `check_original_db3.go`, `check_original_db4.go`, `check_reduced_db.go`, `check_reduced_db2.go`

**Problem:** These one-off analysis scripts have `package main` and `func main()` at the repository root. Running `go build ./...` fails with "main redeclared in this package".

**Workaround:** Build specific commands: `go build ./cmd/observer`, `go build ./cmd/resolver`, etc.

**Fix:** Move to `scripts/analysis/` or `cmd/verify/` with unique package names, or delete after use.

---

### 2. Hash Comparison Bug (Fixed but Documented)
**Location:** `internal/observer/ethhandler/handler.go`, `internal/resolver/resolver.go`

**Problem:** Database stores raw 32-byte `tx_hash` (BLOB). RPC returns hex string with `0x` prefix. Direct comparison `dbHash == rpcHash` always fails.

**Fixed:** Use `hex.EncodeToString(dbHash)` and `strings.TrimPrefix(rpcHash, "0x")` padded to 64 chars for comparison.

**Verification:** Positive control test — 300 blocks known to have matches now show ~100% match rate.

---

## High Priority

### 3. No Integration Tests for Mainnet
**Status:** All tests use in-memory SQLite and `p2p.MsgPipe` mock peers.

**Gap:** No tests against real Ethereum mainnet or local testnet (Kurtosis, Geth devnet).

**Risk:** Protocol handshake, fork detection, and RPC integration untested in CI.

---

### 4. Observer Has No Health/Readiness Endpoints
**Status:** Observer only exposes P2P port (30306). No HTTP health check, metrics endpoint, or pprof.

**Impact:** Cannot integrate with Kubernetes liveness/readiness probes or Prometheus scraping.

**Suggestion:** Add optional HTTP server with `/healthz`, `/metrics` (Prometheus), `/debug/pprof`.

---

### 5. Resolver RPC Endpoint Required at Runtime
**Status:** `ResolverRPCEndpoint` must be configured (config file or `--rpc-endpoint` flag). No default.

**Impact:** Resolver fails fast if RPC endpoint unreachable. No fallback or multiple endpoint support.

**Suggestion:** Support multiple RPC endpoints with failover, configurable timeouts per endpoint.

---

### 6. Block Resolver Assumes Finalized Blocks Available
**Status:** Uses `eth_getBlockByNumber` with `"finalized"` tag.

**Risk:** Some RPC providers don't support `finalized` tag, or finality lag exceeds `post_run_margin`.

**Suggestion:** Configurable block tag (`finalized`, `safe`, `latest`), validation that tag is supported.

---

## Medium Priority

### 7. No Configuration Validation
**Status:** `config.Load()` applies defaults silently. Invalid values (negative timeouts, zero batch size) cause runtime panics or undefined behavior.

**Example:** `DialRatio = 0` → division by zero in dial logic.

**Suggestion:** Add `Validate() error` method to Config, call in `Load()` and at command start.

---

### 8. Single Observer ID, No Multi-Instance Coordination
**Status:** `ObserverID` is a static config field. Running multiple observers requires manual ID assignment.

**Risk:** Duplicate `observer_id` in `collection_runs` breaks provenance.

**Suggestion:** Auto-generate UUID if empty, or derive from hostname + PID + timestamp.

---

### 9. Transaction Dissemination Not Implemented
**Status:** `TxDisseminationEnabled` config exists but `TxDisseminationRateLimit` only limits relay rate. Actual relay logic (`handleNewPooledTransactionHashes` → `GetPooledTransactions` → broadcast) is incomplete.

**Impact:** Fetch mode (`--mode fetch`) requests pooled transactions but doesn't relay to peers.

---

### 10. No Graceful Degradation on RPC Failures
**Status:** Resolver treats all RPC errors equally (retry with backoff). No distinction between transient (timeout, 5xx) and permanent (4xx, invalid response) errors.

**Impact:** Permanent errors waste `max_attempts` retries.

**Suggestion:** Classify errors, stop retrying on permanent failures.

---

### 11. Analyzer Output Not Streaming
**Status:** `runLongitudinalAnalysis` and `runImpactAnalysis` accumulate all rows in memory (`[]string`) before writing output.

**Risk:** Large runs (millions of transactions) cause OOM.

**Suggestion:** Stream rows to output file/handle incrementally.

---

### 12. DBInspect Loads Entire Result Sets
**Status:** `printTxObservations`, `printTrackedTransactions` use `rows.Next()` loop but could fetch large result sets.

**Mitigation:** Currently acceptable for inspection tool, but add `LIMIT` option for safety.

---

## Low Priority / Technical Debt

### 13. Hardcoded go-ethereum Version in Defaults
**Location:** `internal/config/config.go:84`

**Issue:** `GethLibVersion: "go-ethereum/v1.17.5"` hardcoded. Should be injected at build time.

---

### 14. No Structured Logging
**Status:** Uses `github.com/ethereum/go-ethereum/log` (glog-style). No JSON output, no correlation IDs.

**Impact:** Hard to parse in log aggregation systems (ELK, Loki).

**Suggestion:** Add JSON handler option, structured fields for run_id, peer_id, tx_hash.

---

### 15. Config Hash Computation Not Exposed
**Status:** Config hash (SHA256 of effective config) computed in observer but not exposed via CLI or config file.

**Impact:** Cannot verify config hash externally without running observer.

**Suggestion:** Add `--print-config-hash` flag or `config.Hash()` function.

---

### 16. Peer Connection Age Uses Wall Clock
**Status:** `peer_conn_age_ms = (observation_time - handshake_time) / 1e6` uses `time.Now()`.

**Risk:** Clock adjustments (NTP, leap seconds) cause negative or incorrect ages.

**Suggestion:** Use monotonic clock (`time.Since(handshakeTime)`).

---

### 17. No Database Migration Strategy
**Status:** `SchemaVersion` field exists but no migration code. Schema changes require manual SQL.

**Risk:** Breaking changes to schema break existing databases.

**Suggestion:** Add migration framework (golang-migrate or custom), versioned SQL files.

---

### 18. Large Binary Size
**Status:** Binaries are 25-30MB (static linking, debug symbols).

**Optimization:** Build with `-ldflags="-s -w"` to strip symbols (~50% reduction).

---

### 19. No Fuzz Testing
**Status:** No fuzz tests for RLP decoding, message parsing, or RPC response handling.

**Risk:** Malformed peer messages could panic observer.

**Suggestion:** Add `go test -fuzz` for `ethhandler` message decoding.

---

### 20. Windows Path Handling in DB URI
**Location:** `cmd/dbinspect/main.go:134-142`

**Issue:** `readOnlyURI()` converts backslashes to forward slashes for SQLite URI. Tested on Windows but not Linux/macOS.

**Risk:** Potential path issues on non-Windows platforms.

---

## Deferred / Won't Fix

### 21. Simnet/Docker Testbed in Separate Repo
**Status:** Phase 1-3 Docker testbed (Alpine/Geth containers) documented in old README but not maintained here.

**Decision:** Keep dropper-lab focused on observer/resolver/analysis. Simnet in separate repo.

---

### 22. No Transaction Propagation Visualization
**Status:** Analyzer outputs CSV. No built-in visualization (graphs, timelines).

**Decision:** Export CSV → use external tools (Python/Plotly, Grafana, Timesketch).

---

## Fixed in This Version

| Issue | Fix |
|-------|-----|
| Hash comparison (raw bytes vs hex) | `hex.EncodeToString()` + padding |
| `gofmt` formatting | `gofmt -w .` applied |
| `go vet` warnings | Clean pass |
| Test timeouts | `-short` flag skips e2e tests |
| Positive control verification | 300-block sample: 100% match rate |
| VACUUM INTO copy verification | Exact row count match |

---

## Tracking

Issues are tracked in this file. For code review, check:
- `go vet ./internal/... ./cmd/...` — static analysis
- `go test -short ./internal/... ./cmd/...` — unit tests
- `gofmt -l .` — formatting (should be empty)