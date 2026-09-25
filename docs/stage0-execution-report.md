# Ethereum Dropper — Stage 0 Execution Report

**Status:** Stage 0 complete. Stopped at the stage gate; awaiting explicit approval for Stage 1 (observer core).
**Date:** 2026-09-25
**Prerequisite:** `docs/phase0-investigation-report.md` (read in full and confirmed; no contradictions found).
**Nature of this stage:** repository/bootstrap work only. **No observer code, no P2P traffic, no research data yet** — nothing in this report is a measurement.

---

## 1. Approved decisions carried into execution (not re-derived)

Per the project decisions already in force: Go with `github.com/ethereum/go-ethereum v1.17.5` pinned as a normal module dependency (not a fork, not the older local clone at `simnet-work/go-ethereum`); SQLite via pure-Go `modernc.org/sqlite`, WAL mode, batched inserts; native Windows binary for the observer (WSL2 only retained for the localnet rig); two collection profiles (`passive` default for the Stage 3 pilot, `fetch` opt-in); the four-table schema from report §7.

## 2. Actions performed

### 2.1 Version control established (was absent — report §1.1 finding #7)

- `git init -b main` in `C:\Users\kingd\ethereum-dropper-lab`.
- **Commit `07a8609`** — baseline snapshot of the lab exactly as audited: compose localnet, scripts, results snapshots, investigation report, testnet keystores + password files (chainId-1337 only, no mainnet value, kept per the commit spec).
- **One flagged deviation from "exactly what's currently there":** `nodes/node1_test4/geth.ipc` is an OS **named pipe**, not a file — git cannot store it (`error: unable to index file … Function not implemented`). Excluded via `.gitignore` (`*.ipc`). This exclusion is also forward-correct: every localnet start recreates a live IPC named pipe inside the volume-mounted `nodes/` directories, and those can never be committed. Everything else is in the commit.
- Historical note: this repo now contains testnet account keystores and their password files. They protect nothing of value (private chain only), but if repo-sharing ever happens, a credential purge + history rewrite should happen first.

### 2.2 Stray containers stopped

The unrelated `geth` and `lighthouse` containers that were stuck in a restart loop (report §1.1) were stopped. Post-stop state (verified with `docker ps -a`): both `Exited`; the four `ethdropper_node*` containers remain (untouched); the localnet compose setup is fully retained as the Stage 2 validation rig.

### 2.3 Go module pinned and toolchain verified end-to-end

- `go mod init github.com/kingd/ethereum-dropper-lab` — `go 1.26.5`, windows/amd64 (Go at `C:\Go\bin`, off PATH; exported per command).
- Dependencies (`go.mod require`): `github.com/ethereum/go-ethereum v1.17.5` and `modernc.org/sqlite v1.59.0`. `go list -m` confirms resolution to `v1.17.5`, which is tag `v1.17.5` = commit `9621c6ad10934a01b5514886fb6fbd87640b6c05` — **the same commit as the Docker image audited in the Phase 0 report (§1.3)**. The older local master clone was not used anywhere.
- **`cmd/toolcheck`** — the Stage-0 gate binary, written to prove the toolchain and both dependencies end-to-end *before any observer logic exists*. It (1) asserts `eth.ProtocolVersions == [ETH72, ETH71, ETH70, ETH69]` (matching report §2.1), (2) asserts `params.MainnetChainConfig.ChainID == 1`, (3) opens a pure-Go SQLite DB, executes DDL and queries `sqlite_version()`, exiting non-zero on any failure.
- All three verifications passed, executed (not assumed):
  - `go build ./...` — clean
  - `go run ./cmd/toolcheck` →
    ```
    toolcheck ok
      mainnet chainID:        1
      eth protocol versions: [72 71 70 69]
      sqlite (pure-go):      3.53.4
    ```
  - `go vet ./...` — clean
- **Commit `71c3186`** — module files + toolcheck. Working tree clean afterwards: exactly two commits on `main`, `git status` empty.

## 3. Verification trail

| Claim | Evidence |
|---|---|
| go-ethereum dependency is exactly v1.17.5 | `go list -m github.com/ethereum/go-ethereum` → `v1.17.5`; tag↔commit match to Docker image established in phase0 report §1.3/§10 |
| Toolchain works end-to-end on windows/amd64 | toolcheck build+run output quoted in §2.3; vet clean |
| Named-pipe exclusion was necessary, not cosmetic | raw git error: `error: open("nodes/node1_test4/geth.ipc"): Function not implemented` |
| Stray containers stopped | `docker ps -a` shows `geth` / `lighthouse` Exited; only one `*.ipc` artifact exists in the tree (searched, `find . -name "*.ipc*"`) |
| Baseline contains what was specified | `git log --stat 07a8609`; only exclusion documented in §2.1 |

## 4. Research-integrity notes for this stage

- No measurements were taken and none are claimed. Peer-eviction tolerance, announcement rates, coverage — all remain **unknown until the Stage 3 pilot measures them** (report §9.1).
- Provenance labels not yet exercised: no `collection_runs` rows exist yet because no run has started.
- Nothing was rewritten in the Phase 0 report; it remains the single source of truth for architecture and schema.

## 5. What is ready next (NOT started — awaiting approval)

Stage 1 scope per report §8, unchanged: devp2p handshake with static mainnet config + live fork-id (EXTERNAL-labeled block number/time source), eth/69–72 receive loop (`NewPooledTransactionHashes`, `Transactions`; `PooledTransactions` only in fetch mode), `SubscribeEvents` → `peer_events`, announcements/direct broadcasts → `transaction_observations` (duplicates preserved), honest empty `Get*` replies in passive mode, batched WAL SQLite writer, and the run manager populating `collection_runs` (geth_lib_version, go_build_version, collector_git_commit, config_hash, schema_version, mode, data_label).

**Open question to resolve during Stage 1 design (flagged, not decided):** how to record discv4 outcomes for unreachable peers and peers that turn out not to speak eth. Planned default: every dial/discovery outcome goes to `peer_events` as an explicit event type (e.g. `dial_failed` with reason, `handshake_rejected`/`eth_capability_missing`) so nothing disappears silently — exact vocabulary to be proposed with the Stage 1 design for sign-off before implementation.

---

*End of Stage 0 report — stopped at the gate. Stage 1 will not begin without explicit approval.*
