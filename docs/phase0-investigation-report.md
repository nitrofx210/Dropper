# Ethereum Dropper — Phase 0–2 Investigation Report

**Status:** First deliverable, presented for mentor approval. No source or configuration files were modified during the investigation (this report file is the only addition).
**Date:** 2026-09-25
**Scope:** repository audit, exact Geth version verification, P2P transaction-path analysis (Phase 1), architecture comparison and recommendation (Phase 2), proposed schema and implementation plan.

---

## 1. Repository audit

### 1.1 `C:\Users\kingd\ethereum-dropper-lab` (the lab proper)

| Item | Finding |
|---|---|
| Git | **NOT a git repository** (`.git` absent). No commit history, no reproducibility pin. |
| Geth fork | None. |
| Go code | None. |
| Custom P2P component | None. `configs/p2p/` exists but is **empty**. |
| Collector / tx logger / SQLite / CSV code | None. Only bash scripts polling `admin.peers` (`scripts/healthcheck.sh`) and the broken `personal.unlockAccount` tx-generation scripts (`generate_tx.sh`, `measure_tx.sh`). Results are ad-hoc JSON snapshots in `results/`. |
| Docker | 4-node private net (chainId 1337) via `compose.yml`, image `ethereum/client-go:stable`. Uses stale `--coinbase` flag, HTTP/WS wide open, `personal` API requested in flags but **not compiled into this Geth** (documented in `SOLUTION_SUMMARY.md` — the intentionally-abandoned signing workflow). |
| State | `ethdropper_node1–4` exited 2 weeks ago. Also on the machine: unrelated `geth` and `lighthouse` containers stuck in a **restart loop** (wasting resources; worth stopping). |

### 1.2 Surrounding workspace (prior research work)

| Directory | What it is | Relevance |
|---|---|---|
| `C:\Users\kingd\simnet-work\go-ethereum` | Full go-ethereum source clone, **master `06b23b4` (2026-07-15)**, self-identifies `1.17.5-unstable`. Two small uncommitted local mods (`p2p/config.go`, `p2p/server.go`): add an optional `ListenFunc` hook so geth's P2P server can listen over a simulated transport. Nothing tx-related. | **Key asset** — but **12 days older than the Docker image commit**. The `geth-simnet` module pins it via `replace ../go-ethereum`. |
| `C:\Users\kingd\simnet-work\geth-simnet` | Working Go harness that runs **go-ethereum's real `p2p.Server` with a custom subprotocol over simulated links** (two-node test passes per project logs). | Direct precursor to a lightweight observer — proven pattern, same authors/machine/toolchain. |
| `C:\Users\kingd\simnet-work\ethp2p`, `simnet`, `simnet-eclipse` | Prior simulation work (libp2p/QUIC erasure-coded broadcast sims; packet-level sim; eclipse threat model). | Reusable later for `CONTROLLED_SIMULATION` replay; not needed for mainnet collection. |

### 1.3 Environment versions (verified, not assumed)

| Component | Exact value | How verified |
|---|---|---|
| Geth in Docker | **1.17.5-stable**, commit **`9621c6ad10934a01b5514886fb6fbd87640b6c05`** (2026-07-27), built with **go1.26.5**, linux/amd64 | `docker run --rm ethereum/client-go:stable geth version` |
| Docker Server | **29.8.0** | `docker version` |
| Image | `ethereum/client-go:stable` = `b8ab3451a117` (built 2026-07-27) | `docker images` |
| GitHub tag `v1.17.5` | resolves to the **same commit `9621c6ad`** → the image *is* the v1.17.5 release | `git ls-remote origin refs/tags/v1.17.5` |
| Go toolchain | **go1.26.5 windows/amd64** at `C:\Go\bin` (not on PATH); WSL Ubuntu has no Go | `go version` |
| WSL2 | Ubuntu 26.04 LTS, ~7.7 GB RAM visible in WSL | `wsl -l -v` |
| Disk | **94 GB free** on C: (81% used) | `df -h` |

**Audit conclusion:** no existing collector anywhere in the project. The assets that matter are (a) the geth-simnet P2P harness pattern, (b) Docker tooling, (c) the 4-node localnet for later validation, and (d) a go-ethereum source clone that is *close to* but *not exactly* the runtime version.

---

## 2. How this exact Geth (1.17.5, `9621c6ad`) receives pooled transactions

All citations are relative to the go-ethereum v1.17.5 tree (clone of tag `v1.17.5`, commit `9621c6ad`, verified identical to the Docker image commit).

### 2.1 Protocol surface (NOT the eth/66/68 of older tutorials)

`eth/protocols/eth/protocol.go:33-49`: this version negotiates **eth/69, 70, 71, 72** (`ProtocolVersions = [ETH72, ETH71, ETH70, ETH69]`). Relevant messages (`protocol.go:57-76`):

| Code | Message | Carries |
|---|---|---|
| 0x00 | `Status` | version, networkID, genesis, ForkID, **block range** (Earliest/Latest/LatestBlockHash — new in eth/69+, replaces TD/Head) |
| 0x02 | `Transactions` | full signed txs (broadcast) |
| 0x08 | `NewPooledTransactionHashes` | announce: `Types []byte`, `Sizes []uint32`, `Hashes []common.Hash` (+ `Mask` custody bitmap on eth/72) |
| 0x09 / 0x0a | `GetPooledTransactions` / `PooledTransactions` | announce→fetch retrieval, request-ID keyed |
| 0x11–0x15 | `BlockRangeUpdate`, `BlockAccessLists*` (EIP-8159), `GetCells`/`Cells` (PeerDAS cell custody) | block/blob-DA plumbing |

Transport: standard devp2p base protocol v5 over RLPx with snappy compression; max protocol message 10 MB (`protocol.go:52`).

### 2.2 Inbound announcement path

1. `handleNewPooledTransactionHashes` / `handleNewPooledTransactionHashes72` — `eth/protocols/eth/handlers.go:566`, `:586`. Gated by `backend.AcceptTxs()`; decodes the packet; calls `peer.MarkTransaction(hash)` per hash (per-peer known-set); forwards `backend.Handle(peer, ann)`.
2. Backend dispatch — `eth/handler_eth.go:61` (`ethHandler.Handle`), cases at `:64-76`: `h.txFetcher.Notify(peer.ID(), packet.Types, packet.Sizes, packet.Hashes)`. On eth/72, blob-custody hashes additionally go to `h.blobFetcher.Notify(peer.ID(), hashes, packet.Mask)`.
3. Fetcher — `eth/fetcher/tx_fetcher.go:244` (`TxFetcher.Notify`): registers each hash as announced **by that peer string ID**, schedules `GetPooledTransactions` fetches round-robin across announcers (max 256 hashes / 128 KB per request, `tx_fetcher.go:51-52`), with staged fallback to other announcers on timeout/refusal.

### 2.3 Inbound full-transaction path

Two ways full signed txs arrive:

1. **Direct broadcast** — `TransactionsMsg` 0x02 → `handleTransactions` (`handlers.go:639`, max 5000 txs/msg) → `ethHandler.Handle` case `*eth.TransactionsPacket` (`handler_eth.go:78-86`) → local `handleTransactions` (`handler_eth.go:121`) marks known + validates → `h.txFetcher.Enqueue(peer.ID(), peer.Version(), txs, false)`.
2. **Fetched response** — `PooledTransactionsMsg` 0x0a → `handlePooledTransactions` (`handlers.go:655`, request tracked via `peer.tracker.Fulfil` to catch unsolicited/late responses) → `handler_eth.go:88-96` → `h.txFetcher.Enqueue(peer.ID(), …, true)`.

`TxFetcher.Enqueue` (`tx_fetcher.go:335`) then pushes accepted txs into the pool via the `addTxs` callback → `TxPool.Add` (`eth/handler.go:201-203`).

### 2.4 The two gates a collector must understand

- **`AcceptTxs()`** (`eth/handler_eth.go:55-57`) returns `h.synced`, set **only** by `enableSyncedFeatures()` (`eth/handler.go:588-590`) after the downloader finishes initial sync. An unpatched Geth that never syncs **silently drops all transaction traffic** (handlers return before parsing).
- **Peer-blind pool**: `core/txpool/txpool.go:318` — `TxPool.Add(txs []*types.Transaction, sync bool)`; the `sync` flag concerns reorg bookkeeping, **not network origin**. `core.NewTxsEvent{txs}` (`core/events.go:27`) likewise carries no origin. **Once a tx enters the pool, the node forgets the source peer.** This is why `txpool`/`eth_subscribe`/`debug` RPC cannot answer the research question.
- Outbound (what peers send an observer) — `eth/handler.go:517-571` (`BroadcastTransactions`): ordinary txs are sent **as full bytes to a √N, sender-stratified subset** of peers, and **announced as hashes to every other peer** they don't believe already knows the tx; **blob txs and txs > 4096 bytes are announce-only** (`txMaxBroadcastSize`, `eth/handler.go:63`; blob case at `:536`).

### 2.5 Blob transactions — how they differ, exactly

Verified in `eth/handler_eth.go:121-137` and `eth/handler.go:533-540`:

- Receiving a **blob tx via direct `Transactions` broadcast is a protocol violation → disconnect** (`handler_eth.go:125-126`).
- Blob txs are **announce-only** (hash + type + size).
- On eth/69–71, the fetched `PooledTransactions` response **must include the sidecar (blobs+proofs)**; sidecar-less or commitment-mismatched → disconnect (`handler_eth.go:131-136`). The full raw blob tx (with blobs) *is* on the wire in response bodies.
- On eth/72, blob hashes are announced with a **custody bitmap** (`NewPooledTransactionHashesPacket72`, `protocol.go:265-270`) and blob *contents* move over the new **`GetCells`/`Cells`** messages (cell-based DA / PeerDAS), handled by a separate `BlobFetcher` (`eth/handler.go:216-237`).

---

## 3. Peer identity, attribution, raw bytes (deliverables 4–6)

**Peer identity** (`eth.Peer` embeds `*p2p.Peer`, `eth/protocols/eth/peer.go:61-87`):
- `peer.ID()` — enode ID (keccak256 of the peer's secp256k1 node pubkey) — available at **every** tx handling site.
- `p2p.Peer.Info().Name` — the peer's client hello string, e.g. `"Geth/v1.17.5-stable-…/linux-amd64/go1.26.5"` → clean `client_name` + `version` split (`p2p/peer.go:561-597`).
- `Node()` (enode URL), `LocalAddr()`/`RemoteAddr()`, `Inbound()` static/trusted flags — same struct.
- Connection lifecycle without patching: **`p2p.Server.SubscribeEvents`** (`p2p/server.go:289`) emits `add`, `drop`, `msgsend`, `msgrecv` events (`p2p/peer.go:75-96`).

**Attribution table (what the protocol actually proves):**

| Event | Attribution | `source_confidence` |
|---|---|---|
| Hash announced to us by peer P at T | **direct** — P's message, P's socket | `direct` |
| Full tx broadcast to us by P at T | **direct** | `direct` |
| PooledTransactions response delivered the bytes, fetched from P | **direct** — request–response keyed by request ID + tracker | `direct` |
| "P first announced hash H among our peers" / "P was the earliest informant" | derived ordering | DERIVED |
| "P authored/originate H" | **not provable** — P only proved knowledge | not permitted; never stored |

**Raw signed transaction bytes:** YES at this layer —
- `Transactions` messages and `PooledTransactions` responses carry fully signed txs; geth decodes them (`TransactionsPacket` is `rlp.RawList[*types.Transaction]`, `protocol.go:108-111`); exact wire bytes can be captured before decode or losslessly re-encoded from the decoded object.
- Blob txs: full bytes **with sidecar** arrive via fetched responses on eth/69–71; on eth/72 blob content arrives as cells, so full blob-tx reconstruction requires cell assembly (documented limitation, §9).
- **Replayability caveat:** an observer that never issues `GetPooledTransactions` receives only the √N-share of direct broadcasts → partial raw coverage. Full replay-grade capture requires normal-node hash retrieval. This is an explicit mentor decision (see §6, two collection profiles).

---

## 4. Lightweight observer without chain sync — feasibility (deliverable 7)

**Yes — verified against this source.** Note there is **no light client at all**: `eth/ethconfig/syncmode.go:25-32` allows only `full` or `snap`; LES protocol directories do not exist in `eth/protocols/` (only `eth`, `snap`). The option space never included a "Geth light node."

What a handshake peer actually requires (`eth/protocols/eth/handshake.go:67-94`):
1. Matching `NetworkID`, matching negotiated protocol version, matching genesis hash;
2. A **valid ForkID** — computable **offline** from the static mainnet chain config + genesis + a plausible head number/time (`forkid.NewID(chain.Config(), genesis, latest.Number, latest.Time)`, `handshake.go:42`). No chain database needed.
3. Block range: only structurally validated (`EarliestBlock ≤ LatestBlock`, `LatestBlockHash ≠ 0`, `handshake.go:155-163`). No proof of possession.

A minimal observer needs: mainnet identity (keypair + known mainnet genesis hash), the static chain config (shipped in `params/`), and an honest current block range (RPC-sourced). Serving side: reply empty-but-well-formed to `Get*` requests — what an honest node without data does. The unknown only a pilot can answer: how long peers tolerate a non-serving peer. That is an empirical finding, not a protocol violation.

---

## 5. Architecture comparison (deliverable 8)

| | A. RPC-only (`eth_subscribe`, `txpool`) | B. Instrumented Geth fork | C. Lightweight go-ethereum observer | D. Other (Portal, ethp2p, other clients) |
|---|---|---|---|---|
| Peer attribution of tx events | **Impossible** — pool is peer-blind (§2.4). Fails the core research question | Direct | **Direct** (§3) | Portal carries no mainnet tx gossip; other full clients = weight of B; ethp2p is libp2p, not devp2p |
| Duplicate observations (tx from 5 peers) | Invisible (pool dedups) | Visible | Visible | — |
| Raw signed bytes | Only after local pool add — post-dedup | Wire-exact | Wire-exact for what arrives/fetches | — |
| Chain sync needed | No — but then it sees nothing (`AcceptTxs` gate) | **Yes** — full/snap mainnet store; fork upkeep | **No** | — |
| Fits this machine | (collects nothing relevant) | **No** — 94 GB free cannot hold a mainnet node store; 7.7 GB RAM marginal | Yes — footprint is DB size | — |
| Intrusiveness | Passive | A normal full node (fetches/serves) + patches | Passive-receiver (or fetch mode, mentor decision) | — |
| Engineering cost | Trivial (wrong signal) | Fork maintain + full ops | Single Go binary; reuses proven geth-simnet pattern; go-ethereum **as tagged module library, not fork** | — |
| Key risk | Invalid data for research question | Banned by constraint; machine can't host it | Peer churn toward non-serving node (pilot unknown; mitigations: honest empty replies, valid status/fork-id, fresh peer list via discv4) | — |

**Option A is disqualified by verified source behavior, not preference. Option B directly contradicts the mentor's guidance (and the machine). Option C is the first option that holds.**

---

## 6. Recommended architecture (deliverable 9)

**Option C — a lightweight, passive Mainnet tx-gossip observer**, a standalone Go binary using `github.com/ethereum/go-ethereum v1.17.5` **as a library dependency pinned by tag** (no fork of geth itself):

```
                mainnet devp2p (RLPx + eth/69..72 + discv4 peer discovery)
                        │  outbound dials + inbound accepts
                        ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │ observer core (Go: p2p.Server + custom minimal "eth" protocol)   │
   │  - Status/handshake: static mainnet params + live fork-id       │
   │  - receives: NewPooledTransactionHashes(71/72), Transactions,   │
   │              PooledTransactions (fetch mode only)               │
   │  - SubscribeEvents → peer lifecycle records                     │
   │  - honest empty replies to Get* requests                        │
   └───────────────┬──────────────────────────────┬──────────────────┘
                   ▼                              ▼
          SQLite (WAL): raw events        inclusion resolver (separate
          peer_events / tx_observations / phase, external lookup)
                   │                              ▼
                   ▼                       external JSON-RPC (primary)
          derived workload export          Etherscan (optional secondary)
```

Design decisions, each tied to source facts:
- **Announcements are the primary research signal** — every peer announces nearly every new pooled tx to all its peers; this is exactly the `peer → tx → timestamp` temporal graph the study needs. Direct `Transactions` broadcasts (√N per sender) are a bonus tier.
- **Two collection profiles behind one flag** (mentor decision): `passive` (receive-only; strictly connection-maintenance passive) and `fetch` (ordinary hash→bytes retrieval so the workload is replay-grade). The pilot reports first-seen coverage so the decision is data-driven.
- **Reuses the `geth-simnet` pattern already proven in this project** — same authors, machine, toolchain (go1.26.5, matching the version Geth itself was built with).
- **Peer discovery:** dial `params.MainnetBootnodes` (`params/bootnodes.go:23`) into discv4, collect nodes advertising the `eth` capability. Config-driven max-peers (start ~30).
- **Blob txs:** record announcements fully (hash/type/size/custody-mask); raw blob capture only via fetch mode (documented limitation).
- **Status currency:** fetch `eth_blockNumber` (EXTERNAL, labeled) periodically so the advertised block range stays honest. **The collector must not be described fully "passive" if fetch mode is on** — fetch mode does exactly what every normal node does (hash retrieval), nothing more.
- **Existing 4-node localnet becomes the validation rig** (`LOCAL_TESTNET` label), *before* any `MAINNET_OBSERVATION` run.

---

## 7. Proposed SQLite schema (deliverable 10)

Design constraints: raw events append-only, duplicates preserved, provenance explicit, no silent dedup. Full DDL will go in `docs/data-schema.md` after approval.

**`peer_events`** — lifecycle & identity (from `SubscribeEvents` + handshake):
```sql
event_id INTEGER PRIMARY KEY,
run_id INTEGER NOT NULL,
observer_id TEXT NOT NULL,
peer_id TEXT NOT NULL,              -- enode ID hex (trigger: non-empty for peer events)
event_type TEXT CHECK (event_type IN ('connect','disconnect','handshake_ok')),
direction TEXT CHECK (direction IN ('inbound','outbound')),
timestamp_ns INTEGER NOT NULL,
client_name TEXT,                   -- parsed from devp2p hello
client_version TEXT,
enode TEXT,
remote_addr TEXT,
eth_version INTEGER,
detail_json TEXT
```

**`transaction_observations`** — one row per (peer, tx, message):
```sql
observation_id INTEGER PRIMARY KEY,
run_id INTEGER NOT NULL,
observer_id TEXT NOT NULL,
peer_id TEXT NOT NULL,
tx_hash BLOB NOT NULL,
timestamp_ns INTEGER NOT NULL,
message_type TEXT CHECK (message_type IN ('announcement','direct_broadcast','pooled_tx_response')),
eth_version INTEGER NOT NULL,
tx_type TINYINT,                    -- from Types byte when available
announced_size INTEGER,             -- from Sizes when available
raw_transaction BLOB,               -- exact wire bytes if available, else NULL
source_confidence TEXT CHECK (source_confidence IN ('direct','inferred','unknown'))
    NOT NULL DEFAULT 'direct',
custody_mask BLOB                   -- blob/eth72 only
-- INDEX (tx_hash, timestamp_ns); INDEX (peer_id, timestamp_ns)
```
All duplicates retained: same tx from 5 peers = 5 rows. That is the data.

**`transaction_inclusion`** — external lookups:
```sql
tx_hash BLOB PRIMARY KEY,
status TEXT CHECK (status IN ('not_found_yet','included','not_included_within_window','lookup_failed')),
block_number INTEGER,
block_hash BLOB,
block_time INTEGER,
first_confirmed_ns INTEGER,
inclusion_delay_ns INTEGER,         -- DERIVED: first_confirmed_ns - first_seen_ns
lookup_source TEXT CHECK (lookup_source IN ('rpc','etherscan')),
last_lookup_ns INTEGER,
lookup_attempts INTEGER
```
Never conflate `not_found_yet` with `not_included_within_window`.

**`collection_runs`** — one row per run:
```sql
run_id INTEGER PRIMARY KEY,
start_ns INTEGER, end_ns INTEGER,
observer_id TEXT,
geth_lib_version TEXT,             -- 'v1.17.5 / 9621c6ad...'
go_build_version TEXT, os TEXT,
collector_git_commit TEXT, config_hash TEXT, schema_version TEXT,
mode TEXT CHECK (mode IN ('passive','fetch')),
data_label TEXT CHECK (data_label IN ('MAINNET_OBSERVATION','LOCAL_TESTNET','CONTROLLED_SIMULATION')),
notes TEXT
```

SQLite via pure-Go driver (`modernc.org/sqlite`, no cgo — clean on Windows), WAL mode, batched inserts. Phase 10/11 workload is exported **views** over the raw tables — derived data never overwrites raw.

---

## 8. Implementation plan (deliverable 11)

| Stage | Content | Gate |
|---|---|---|
| 0 | `git init` the lab repo (currently none), pin go-ethereum `v1.17.5` tag as module dependency | mentor approval |
| 1 | Observer core: devp2p connect/handshake (static mainnet config, live fork-id, honest status), eth/69–72 receive loop, `SubscribeEvents` → `peer_events`, announcement/direct-broadcast → `transaction_observations`, WAL SQLite writer, run manager | Stage 2 agreement |
| 2 | **LOCAL_TESTNET validation** against the 4-node cluster with ground-truth injected txs | full attribution agreement on known txs |
| 3 | **MAINNET_OBSERVATION pilot, 6–24 h, passive mode**; data-quality report (peer count/diversity, observations/s, unique vs duplicate txs per hash, restart-resume, eviction behavior, sample inclusion check) | pilot must show: multiple peers, valid hashes, same tx from multiple peers, safe restart |
| 4 | Inclusion resolver as separate tool (`InclusionResolver` interface, `RpcInclusionResolver` primary, `EtherscanInclusionResolver` secondary; retry/backoff; 4-state persistence) | sample correctness; mentor decides `fetch`-mode question |
| 5 | Multi-day runs → derived workload export → analysis scripts (Phase 11 metrics) → docs suite (`data-collection-plan.md`, `architecture.md`, `data-schema.md`, `runbook.md`, `research-limitations.md`) | pilot sign-off |

Operational choice stated up front: observer runs as a **native Windows Go binary** (WSL2 sleep behavior is unreliable for 24/7; binary has no cgo/systemd needs); Docker remains for the localnet only.

---

## 9. Risks & limitations (deliverable 12)

*(categories per Phase 16: OBSERVED / DERIVED / EXTERNAL / INFERRED)*

1. **Peer-eviction behavior of a non-serving node is unknown** (measured in pilot; protocol-legal but empirically unmeasured). Mitigations: honest empty responses, valid handshake/fork-id, fresh peer list via discv4. Fallback if disqualifying: Option-B-lite (patched real geth) — but that requires the full chain we are told to avoid.
2. **ForkID lifetime**: locally derived IDs go stale when Mainnet schedules new forks. Mitigation: derive from chain config + refresh head/time from external RPC; alert on mass handshake rejections.
3. **Direct-broadcast fan-out is √N and sender-stratified** (`eth/handler.go:543-544`): a passive observer sees announcements from all peers but raw bytes only for its share of direct transfers. Coverage must be reported as a measured bias, not hidden.
4. **Blob tx replayability** limited on eth/72 cells (OBSERVED announcement + partial raw unless fetch mode / cell assembly; documented, not fabricated).
5. **Clock**: wall + monotonic; NTP-synced; never trust cross-peer ordering before same-source monotonicity checks.
6. **"Source peer" semantics**: we record "peer X knew/sent us H at T" — never "X authored H". Fabricating origination is forbidden.
7. **`geth`/`lighthouse` containers in restart loop** on this machine and **no git repo for the lab**: both cheap to fix, both block reproducibility/24-7 reliability if ignored.
8. **Disk claim**: 94 GB free (measured). Exact Mainnet store sizes not independently re-verified this session (web tools unavailable) — but even the lowest documented geth store norms are multiples of free space; the no-full-node conclusion holds qualitatively.
9. **Ethics/safety**: passive by design; in `fetch` mode the node performs ordinary hash retrieval only — no new/malformed messages, no spam, no eclipse/evasion logic, no RPC/admin endpoints on the observer.

---

## 10. Verification trail (no-guesswork rules)

- Docker Geth version: `docker run --rm ethereum/client-go:stable geth version` → 1.17.5-stable / `9621c6ad…` / go1.26.5.
- Tag↔image match: `git ls-remote origin refs/tags/v1.17.5` → `9621c6ad…` (identical).
- All §2–§4 facts read directly from a clean clone of tag `v1.17.5` at `9621c6ad` (kept outside the project tree; zero project modification during investigation).
- Old-tutorial assumptions explicitly rejected where source disagreed (eth/66-68, TD-based Status, `personal` API, light mode — all absent/removed in this version).
- No measurements fabricated; pilot-dependent questions are labeled as unknown-until-measured.

---

## Pending mentor decisions

1. Approve Option C (lightweight observer) as the collection architecture.
2. Stage 3 pilot mode: **passive-only first (recommended)**, with `fetch` mode only if raw-byte coverage proves insufficient — or `fetch` from day one.
3. Consent to `git init` the lab directory for reproducibility.

*End of report — awaiting approval. No implementation work has begun.*
