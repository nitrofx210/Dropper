Subject: Ethereum Dropper Project Update - RPC Endpoint Findings & Transaction Inclusion Analysis (Week of 2026-10-11)

Dear [Supervisor's Name],

As requested in our meeting two days ago, please find below my update on the Ethereum Dropper project within the one-week timeline. This email covers the two key action items: RPC endpoint feasibility testing and transaction inclusion time window analysis from the Geth dropper mechanism (PR #34702).

## 1. RPC Endpoint Testing Results

I tested the three public RPC endpoints you specified to determine viability for collecting 48 hours of block data:

**PublicNode (https://ethereum-rpc.publicnode.com)**
- Status: ❌ FAILED
- Error: TLS certificate mismatch (certificate valid for *.airtel.com, airtel.com, not ethereum-rpc.publicnode.com)
- Sustained rate: 0 req/s (100% failure)
- Verdict: Unusable due to certificate issues

**DRPC (https://eth.drpc.org)**
- Status: ✅ RELIABLE
- Sustained rate: 3.80 req/s (tested over 200 consecutive eth_getBlockByNumber requests)
- Median latency: 248.65ms, P95 latency: 327.92ms
- Batch support: ✅ Yes (tested successfully up to size 20)
- Special tags: ✅ Both "safe" and "finalized" block tags supported
- Head lag: < 1 block compared to other endpoints
- 48h window projection: 14,400 blocks / 3.80 req/s = 3,789 seconds = 1.05 hours
- Verdict: Only consistently working endpoint among those tested

**OnFinality (https://eth.api.onfinality.io/public)**
- Status: ❌ FAILED
- Error: Rate limiting (RPC error -32029: "Too Many Requests, Please apply an OnFinality API key...")
- Sustained rate: 0 req/s (100% failure from rate limiting)
- Verdict: Unusable without API key/higher rate limit

**Key Finding**: DRPC provides sufficient throughput for 48-hour data collection (~1 hour backfill time), well within your stated requirement that "rate-limited public nodes should still allow downloading 48 hours of data within a week."

## 2. Transaction Inclusion Time Window Analysis (Geth PR #34702)

Based on my analysis of Geth's dropper mechanism (PR #34702), here's how transaction inclusion is determined and the associated time windows:

### Inclusion Determination Rule
A transaction is counted as "included" when **BOTH** conditions are met:
1. The transaction appears in a canonical block (found via `block.Transactions()`)
2. The transaction's first-seen time is **strictly less than** the block time (`ti.AddedAt < blockTime`)

This is implemented in `eth/txtracker/tracker.go:228-233` with the check:
```go
if !ok || ti.AddedAt >= blockTime { 
    continue 
}
```
which skips transactions delivered at or after the block slot.

### Time Windows via Exponential Moving Averages
The dropper uses two EMAs to score peers based on transaction inclusion:

**RecentIncluded (fast EMA)**:
- α = 0.05 (observed in `eth/txtracker/tracker.go:45`)
- Half-life ≈ ln(2)/0.05 ≈ 13.8 blocks
- At Ethereum's ~12s/block interval: **≈ 2.7 minutes**
- Measures short-term inclusion performance

**RecentFinalized (slow EMA)**:
- α = 0.0001 (observed in `eth/txtracker/tracker.go:50`)
- Half-life ≈ ln(2)/0.0001 ≈ 6930 blocks
- At Ethereum's ~12s/block interval: **≈ 23 hours**
- Measures long-term finalization performance

**Critical Insight**: There is no explicit timeout where transactions are "treated as not included." Instead, inclusion judgment is continuous:
- Older inclusions receive exponentially less weight but are never fully discarded
- Recent inclusions (within ~2.7 minutes) weigh heavily in peer scoring
- Finalized inclusions (within ~23 hours) provide long-term peer protection

### Practical Implications for Our Resolver
My per-block resolver design will:
1. Store both `inclusion_time_ns` (block timestamp) and `finalization_time_ns` (query timestamp)
2. Apply the inclusion rule explicitly: `first_seen_ns < block_timestamp_ns`
3. Use the "finalized" block tag for reorg safety (DRPC supports this per testing)
4. Implement a single post-run margin (configurable, default 1 hour) measured from run end

## 3. Project Status & Next Steps

**Completed Work**:
- Part 1: Geth dropper mechanism analysis (docs/geth-dropper-analysis.md)
- Part 2: RPC endpoint testing results (docs/rpc-endpoint-findings.md)
- Part 3: Per-block resolver design (docs/resolver-per-block-design.md) - **Awaiting your approval before implementation**

**Current Blockers**:
- Part 3 implementation is paused per your explicit instruction: "stop for approval before implementation"
- The resolver design document incorporates all requested fixes:
  - Uses `eth_getBlockByNumber(n, false)` (returns headers+tx hashes)
  - Defaults to `finalized` tag for reorg safety
  - Includes batching note (DRPC accepts size 20 → reduces 48h backfill to few minutes)
  - Explicit inclusion rule check
  - Margin clarified as measured from run end
  - Marks run_preparation.md and resolver-explanation.md as superseded

**Recommended Path Forward**:
1. Your review and approval of the per-block resolver design (docs/resolver-per-block-design.md)
2. Implementation of cmd/blockresolver based on the approved design
3. Testing with DRPC endpoint for 48-hour historical data collection
4. Evaluation of the two existing Geth peer dropping strategies (random vs. inclusion-based) as discussed

Please let me know if you'd like me to proceed with implementing the per-block resolver once you've reviewed the design, or if you need any adjustments to the approach.

Best regards,
Aryan