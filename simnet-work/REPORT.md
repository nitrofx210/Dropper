# simnet — Run Report

**Date:** 2026-07-16
**Repository:** https://github.com/MarcoPolo/simnet (cloned to `C:\Users\kingd\simnet-work\simnet`)
**Goal:** Set up and run the simnet in-process packet-network simulator.

## What simnet is
A small Go library that simulates packet networks in-process. It provides
drop-in `net.PacketConn` endpoints connected through virtual links with
configurable bandwidth, latency, MTU, and FQ-CoDel bufferbloat mitigation.
Useful for testing networking code without real sockets or root privileges.

## Environment setup
- Go was **not** on PATH; found preinstalled at `C:\Go\bin` (go1.26.5 windows/amd64).
- Added `C:\Go\bin` to PATH for the session.
- Created workspace `C:\Users/kingd\simnet-work`, cloned the repo.
- `go mod download` fetched the single dependency (`golang.org/x/time v0.12.0`).
- Go 1.26.5 satisfies the >=1.24 requirement (synctest tests build under `//go:build go1.25`).

## How it was run
1. **Build** the library: `go build ./...` — succeeded (no errors).
2. **Test suite**: `go test ./...` — all tests PASS.
3. **synctest tests**: These run by default on Go 1.26 (no `GOEXPERIMENT=synctest`
   needed — that flag now errors as "unknown" on 1.26). They verify latency and
   bandwidth accuracy, e.g.:
   - `TestBandwidthLimiter_synctest`: observed 10.0089 Mbps vs expected 10 Mbps (0.0009% error).
   - `TestSimnetWithSynctest`: observed latency 10ms vs expected 10ms (0% diff).
   - `TestSimnetBandwidthWithSynctest`: observed 40.009 Mbps vs expected 40 Mbps.
   - `ExampleSimnet_echo`: echo `ping` -> `echo: ping` — PASS.
   - `TestSimpleHolePunch`, `TestPublicIP`, `TestLinkDriver*`, etc. — all PASS.
4. **Standalone demo** (`examples/echo/main.go`): ran via `go run ./examples/echo`:
   ```
   echo: ping
   client stats: {BytesSent:4 BytesRcvd:10 PacketsSent:1 PacketsRcvd:1}
   server stats: {BytesSent:10 BytesRcvd:4 PacketsSent:1 PacketsRcvd:1}
   ```

## Notes / gotchas
- The README's Quick Start example is **stale** (API changed):
  - `NodeBiDiLinkSettings` no longer has a `Latency` field. Latency is now set
    on the `Simnet` via `LatencyFunc` (e.g. `simnet.StaticLatency(5*time.Millisecond)`).
  - `Simnet.Start()` returns nothing (the example used `n.Start()` as a value).
  - The corrected version is in `examples/echo/main.go`.
- The repo's own `ExampleSimnet_echo` test (`simnet_test.go`) is the canonical
  working usage and passes.

## How to re-run
```bash
export PATH="$PATH:/c/Go/bin"            # if Go not on PATH
cd /c/Users/kingd/simnet-work/simnet
go test ./...                            # full suite incl. synctest
go run ./examples/echo                   # standalone echo demo
```

## Status: COMPLETE — simulator built, tested, and run successfully.
