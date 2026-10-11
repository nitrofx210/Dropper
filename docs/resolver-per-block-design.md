# Per-Block Resolver Design

## 5-line summary for supervisor email
Replace transaction-by-transaction polling with block-based fetching: download blocks via eth_getBlockByNumber(n, false) (headers+tx hashes), lookup hashes in database index. Store results in new transaction_inclusion table while keeping observer DB read-only. Use finalized tag for reorg safety and single post-run margin from run end.

## Design Overview

*Note: This design supersedes previous resolver explanations in docs/run_preparation.md and docs/resolver-explanation.md.*

The current resolver polls `eth_getTransactionByHash` per transaction with exponential backoff, which doesn't fit free-tier rate limits and has too short a retry window to judge inclusion fairly. The proposed per-block resolver downloads blocks instead, extracting all transaction hashes from each block and matching them against observed transactions.

### Core Workflow
1. Determine block range from collection run's start/end times plus configurable buffer
2. Fetch blocks sequentially (or in parallel with rate limiting)
3. Extract transaction hashes from each block
4. Match against observed transactions to determine inclusion
5. Record results with provenance information
6. Continue with next block until reaching the configured end boundary
7. Support live mode for following new blocks as they arrive

## Detailed Design

### Block Range Calculation
- **Start block**: Derived from the collection run's start time in `collection_runs` table
- **End block**: Derived from the collection run's end time plus:
  - `post-run-margin`: single configurable time to wait after run end before considering a transaction definitely not included within window (combines not-included-window and buffer)
- Value configurable via command-line flags or config file

### Block Fetching and Processing
```
FOR each block in range(start_block, end_block):
    // Use batching when possible: fetch up to batch-size blocks per RPC call
    FETCH blocks via eth_getBlockByNumber(_, false) in batches of up to <batch-size>
    // Each block request returns header fields + transaction hashes (not full tx objects)
    IF request fails:
        APPLY rate limiting, retry, and endpoint failover logic
        CONTINUE to next block (or retry same block based on error type)
    FOR each block in batch:
        EXTRACT block_timestamp and transaction hashes from block
        LOOK UP each transaction hash in block_transactions in database index (do NOT loop over all observed transactions per block)
        FOR each matched observed transaction NOT yet finally determined:
            // Apply inclusion rule: first_seen_time < block_time
            IF tx.first_seen_ns < block_timestamp_ns:
                INSERT INTO transaction_inclusion (
                    tx_hash, block_number, block_hash, 
                    inclusion_time_ns, finalization_time_ns,
                    lookup_source, last_lookup_ns, lookup_attempts
                ) VALUES (
                    <tx_hash>, <block_number>, <block_hash>,
                    <block_timestamp_ns>, <query_timestamp_ns>,
                    'rpc_block', <query_timestamp_ns>, 1
                )
            END IF
        END FOR
    END FOR
    OPEN observer database connection AS READ-ONLY to avoid interfering with ongoing writes
FOR each unmatched observed transaction still pending:
    CONTINUE observing (still "not found yet")
```

### State Management and Honest Outcomes
The resolver maintains three distinct states for each transaction:
1. **Not found yet**: Transaction hash not seen in any processed block (default state)
2. **Included**: Transaction found in a canonical block with proper confirmation depth
3. **Not included within window**: Transaction not found after waiting the configured post-run margin beyond observation end

**Critical distinction**: Never treat "not found yet" as evidence of absence. A transaction might still be included in a future block. The post-run margin combines both the not-included-window and buffer into a single configurable time to wait after observation end.

### Reorg Safety
Inclusion is only considered final after:
- Using the "finalized" block tag (DRPC supports this per Part 2 findings)
- OR waiting for a configurable confirmation depth (e.g., 64 blocks ≈ 2 epochs) as fallback

This prevents treating transactions in reorged blocks as finally included.

### Resumability
- Checkpoint last successfully processed block number to disk
- On restart, continue from the checkpointed block rather than restarting from beginning
- Handle partial block processing if interrupted mid-block

### Rate Limiting, Retry and Endpoint Failover
- **Rate limiting**: Back off on HTTP 429 responses, respect Retry-After header if present
- **Retry**: Configurable retry attempts with exponential backoff + jitter
- **Endpoint failover**: Configure multiple RPC endpoints; automatically switch on persistent failures
- All parameters configurable via command-line flags or config file

### Provenance Tracking
For every inclusion result, record:
- **Endpoint used**: The RPC endpoint that provided the block data
- **Query timestamp**: When the block was fetched
- **Block source**: Whether from regular block number or special tag (safe/finalized)
- Label as `EXTERNAL` in storage to distinguish from potential future internal sources

### Live Mode
- Follow new blocks as they appear (approximately every ~12 seconds)
- One call per new block via `eth_blockNumber` to detect chain head
- Process new blocks immediately upon detection
- Maintain same processing logic as historical mode

## Schema Clarification

### Authoritative Schema Today
Based on code review of `internal/storage/models.go` and `internal/storage/sqlite.go`:

The **authoritative schema today** uses the `tracked_transactions` table, **not** a `transaction_inclusion` table.

### Relationship Between Schemas
- `tracked_transactions`: Working table used by the current resolver for exponential backoff polling
  - Contains fields: `first_seen_ns`, `last_checked_ns`, `next_check_ns`, `attempt`, `included`, `block_number`, `block_hash`, `transaction_index`, `useful_score`
  - `included` is NULL/unknown, true/false
- `transaction_inclusion`: Proposed but not implemented external lookup results table mentioned in Phase 0 report
  - Will contain: `tx_hash`, `block_number`, `block_hash`, `inclusion_time_ns`, `finalization_time_ns`, `lookup_source`, `last_lookup_ns`, `lookup_attempts`
  - Status determined by NULL/not NULL values in time fields

**For this implementation**: The per-block resolver will populate the `transaction_inclusion` table with results from external lookup, keeping it separate from the working `tracked_transactions` table. This maintains clear separation between internal observation data and external lookup results.

### Recommended Approach
Not silently renaming or merging anything. Instead:
1. Keep `tracked_transactions` as the primary working table for internal observation data
2. Create and populate `transaction_inclusion` table for external lookup results:
   - `tx_hash` (foreign key to tracked_transactions)
   - `block_number`, `block_hash` (where transaction was found)
   - `inclusion_time_ns` (block timestamp when transaction was mined)
   - `finalization_time_ns` (when we learned about inclusion via RPC)
   - `lookup_source` (e.g., 'rpc_block')
   - `last_lookup_ns` timestamp
   - `lookup_attempts` count
3. Keep the existing semantic meaning of `included`, `block_number`, etc. in tracked_transactions
4. This maintains clear separation between internal observation and external lookup data

## Configuration Values
Make the following configurable (with sensible defaults):
- `post-run-margin`: Time to wait after observation end before considering definitely not included within window (default: 1 hour)
  - Measured from the collection run's end time, not per-transaction first-seen time
- `use-finalized-tag`: Use "finalized" block tag for reorg safety (default: true, as DRPC supports it per Part 2)
- `confirmation-depth`: Blocks to wait before considering inclusion final if not using finalized tag (default: 64 blocks ≈ 2 epochs)
- `batch-size`: Number of blocks to fetch per RPC batch request (default: 20, as DRPC supports batches up to size 20)
  - Reduces 48h backfill time from ~1 hour to a few minutes
- `max-concurrent-requests`: For parallel block fetching (default: 5)
- `rate-limit-retry-attempts`: HTTP 429 retry count (default: 3)
- `endpoint-failover-threshold`: Consecutive failures before switching endpoints (default: 5)

## Interface with Existing Components
- **Reads from**: `collection_runs` table to determine observation time range
- **Writes to**: `tracked_transactions` table to update inclusion results
- **Does NOT modify**: Observer or Analyzer components
- **Interaction**: Separate tool that runs after observation phase completes, or concurrently in live mode