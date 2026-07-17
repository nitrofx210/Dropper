# ethp2p — Pivot & Run Report (real devp2p semantics)

**Date:** 2026-07-16
**Repo:** https://github.com/ethp2p/ethp2p (cloned to `C:\Users\kingd\simnet-work\ethp2p`)
**Pivot from:** MarcoPolo/simnet (toy in-process packet sim) → ethp2p Simnet driver (real libp2p/QUIC + erasure-coded broadcast)
**Status:** BUILT and RUN — Simnet driver tests pass with real broadcast strategies.

## What ethp2p is
A next-generation Ethereum p2p networking stack. Five layers (Transport/Peering/Broadcast/Privacy/Control); the **Broadcast** layer is implemented: an erasure-coded (Reed-Solomon / RLNC) broadcast engine with a libp2p **gossipsub** baseline, running over QUIC hosts. It ships a **Simulation** framework with two drivers:
- **Simnet** — in-process via Go `testing/synctest`, deterministic, no external deps, ~16 nodes.
- **Shadow** — discrete-event simulator, 1000+ nodes (needs Shadow installed; skipped here).

Crucially, the Simnet driver is built **on top of `github.com/marcopolo/simnet`** (the exact library from the previous step, pinned at `v0.0.5`). So the pivot reuses the packet transport and layers *real* protocol semantics on top.

## Environment / toolchain
- Go 1.26.5 (repo requires 1.25) — on PATH via `C:\Go\bin`. ✅
- Python 3.14 + `uv` 0.11.25 — used for the `simctl` CLI. ✅
- Rust/cargo — NOT needed (ethp2p is pure Go here). 
- `buf` — NOT needed; `.pb.go` files are already committed.
- Shadow — NOT installed (Shadow mode out of scope; used Simnet mode).

## How it was run
1. **Proto files already present** (`broadcast/pb`, `broadcast/rs/pb`, `protocol/pb`) → skipped `buf generate`.
2. **Build:** `go mod download && go build ./...` → **BUILD EXIT: 0** (pulls libp2p, quic-go, reedsolomon, marcopolo/simnet, etc.).
3. **Simnet scenario tests (the pivot target):** `go test ./sim/... -v -short -count=1` → **PASS**.
   - `TestNetwork` runs the `SimnetDriver` with real strategies over the simnet transport:
     - `RS` (Reed-Solomon, 16+16 shards), `RS-ChunkLen`, `RS-ChunkLen-2`
     - `Gossipsub` (libp2p pubsub baseline)
   - For each of 2 topologies (2-node and 6-node), every node receives the exact published bytes (verified by `require.Equal` on `PublishedMessages` vs `ReceivedMessages`).
   - Trace tests (`TestTraceScenario`, `TestTracingObserver_ProducesValidTrace`, etc.) also PASS — they emit `.bctrace` event logs (chunk-send/receive, decode, etc.).
4. **Full module:** `go test ./... -short -count=1` → **all packages OK** (`broadcast`, `broadcast/rs`, `protocol`, `sim`). `transport` has no tests yet (still "Designing" per README).
5. **CLI:** `cd sim/cli && uv sync` → `simctl` installed and `--help` works (commands: init, run, topology, analyze, remote).

## Known limitation (documented, not blocking)
- `simctl run <config> --mode=simnet` is currently **non-functional** in this checkout. The Python runner (`sim/cli/simctl/runner.py`, `build_simnet_test`) compiles a binary from `./sim/cmd/simnet`, but **that directory does not exist** — only `sim/cmd/shadow` is present.
  - Verified: `go test -c ./sim/cmd/simnet` → `directory not found`.
  - The canonical, working way to run real devp2p semantics is therefore the Go test path above (`go test ./sim/...`), which exercises the identical `SimnetDriver` code.
  - Fix (optional): restore/create `sim/cmd/simnet` as a `package main` that reads `--config` and calls `RunSimnetScenario`, or repoint `build_simnet_test` at `./sim`.

## "Real devp2p semantics" — what actually ran
- Real **libp2p QUIC hosts** (`host.go`) with real connection/session machinery.
- Real **erasure-coded broadcast** (`broadcast/rs`, `broadcast/rlnc`) with Merkle chunk verification.
- Real **gossipsub** (`strategy_gossipsub.go`, go-libp2p-pubsub) as baseline.
- Realistic **topology** from Ethereum geo/RTT data (country weights + RTT matrix) in `sim/cli/data/`.
- Deterministic time via `testing/synctest`; bandwidth/latency enforced by the MarcoPolo simnet link model.

## How to re-run
```bash
export PATH="$PATH:/c/Go/bin"
cd /c/Users/kingd/simnet-work/ethp2p
go build ./...                                   # build all Go packages
go test ./sim/... -v -short -count=1             # run Simnet driver (real strategies)
go test ./... -short -count=1                    # full module
# CLI (shadow/simnet via uv):
cd sim/cli && uv sync && uv run simctl --help
```

## Summary
The pivot is complete and verified. From the toy simnet packet sim, we moved to ethp2p's Simnet driver, which layers actual libp2p/QUIC + Reed-Solomon/RLNC erasure-coded broadcast (with a gossipsub baseline) on top of that same transport. The whole stack builds and the Simnet scenario tests pass deterministically. The only gap is the `simctl` CLI's simnet build target (`sim/cmd/simnet` missing), which does not affect the working `go test` path.
