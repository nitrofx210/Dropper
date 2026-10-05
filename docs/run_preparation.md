# Mainnet Collection Run Preparation

## Configuration
- **Config file**: `config/mainnet_run.json` (with `tx_dissemination_enabled: true`)
- **Data label**: `MAINNET_OBSERVATION` (set in config, can be overridden by `--label` flag)
- **Mode**: `passive` (default, receive-only)
- **Resolver settings**:
  - `resolver_rpc_endpoint`: MUST be set to a valid Ethereum JSON-RPC endpoint (e.g., Infura, Alchemy, or self-hosted node) before starting the run.
  - `resolver_max_attempts`: 5 (as per config)
  - `resolver_base_delay`: 1s (as per config, exponential backoff with jitter)

## Storage
- **Database path**: `observer.db` (relative to where the command is run, or absolute if set in config)
- **Expected schema**: Includes tables for `collection_runs`, `peer_events`, `transaction_observations`, and `tracked_transactions`.

## Estimated Disk Usage
Based on short test runs, the storage grows roughly with the number of transaction observations.
- In a 10-minute test with mock data, the database was a few hundred KB.
- For a mainnet run observing transaction gossip, we estimate:
  - ~1-5 transactions per second observed (depending on connectivity and mempool activity).
  - Each observation record is ~200 bytes (tx hash, timestamps, peer ID, etc.).
  - Rough estimate: 1-5 tx/sec * 200 bytes/tx * 86400 sec/day = 17-86 MB per day for transaction_observations.
  - The `tracked_transactions` table will grow similarly (one entry per unique transaction hash).
  - Peer events are relatively infrequent (connect/disconnect/handshake_ok).
  - **Total estimated disk usage for 48 hours**: 1-2 GB (conservative upper bound, assuming high observation rate and including indexes and WAL overhead).

## Disk Space Guard
**Note**: The current Go observer does **not** include an automatic disk space guard (unlike the original Python collector). The observer will continue to run and log errors if the disk becomes full, but it will not shut down automatically. It is the operator's responsibility to monitor disk usage and stop the run before the disk is exhausted to avoid database corruption or loss of observations.

## Monitoring Plan
To ensure the run remains healthy without constant supervision:

1. **Process Health**:
   - Check that the observer and resolver processes are still running (e.g., via `ps` or a simple wrapper script).
   - Set up a restart policy (e.g., using a process manager or simple shell loop) in case of unexpected termination.

2. **Log Monitoring**:
   - Redirect stdout/stderr to log files with rotation (e.g., using `logger` or `svlogd`).
   - Watch for patterns:
     - Observer: regular P2P networking logs, peer connection/disconnection events.
     - Resolver: periodic logs about checking transactions (every 5 seconds per due transaction).
   - Set up alerts for:
     - No new logs for >5 minutes (possible hang).
     - Repeated errors (e.g., RPC connection failures, P2P handshake failures, or disk full errors).

3. **Database Growth**:
   - Periodically check the size of `observer.db` (e.g., every hour via a cron job).
   - Expect steady growth; sudden cessation of growth may indicate the observer has stopped observing.
   - **Important**: Watch for errors in the logs indicating "disk full" or similar storage errors. If such errors appear, plan to stop the run soon.

4. **Basic Sanity Checks** (can be automated):
   - Run the analyzer tool on the database every few hours to ensure:
     - New data is being added (increasing run_id or timestamps).
     - The resolver is making progress (check `tracked_transactions` for non-null `last_checked_ns` and decreasing `next_check_ns` values).
   - Example analyzer command:
     ```
     ./analyzer --longitudinal --impact --db=observer.db --output=analysis_$(date +%H%M).csv
     ```

## Expected Behavior
- **Observer**: Should establish outgoing connections to bootnodes and maintain peer count up to `max_peers` (50 by default). With dissemination enabled, it will re-announce observed transactions at a rate limited to 10 msgs/sec.
- **Resolver**: Will check each observed transaction for inclusion via `eth_getTransactionByHash` with exponential backoff (starting at 1s, doubling + jitter each attempt) up to 5 attempts. After that, it stops checking and leaves the inclusion status unknown if the transaction never appeared.

## Run Duration
Proposed: 48 hours (two days) of continuous observation to capture mempool dynamics and transaction status changes.

## How to Start
1. Ensure a valid JSON-RPC endpoint is set in `config/mainnet_run.json` (or via `--rpc-endpoint` flag).
2. Run:
   ```
   ./observer --config=config/mainnet_run.json
   ./resolver --config=config/mainnet_run.json
   ```
   (In separate terminals or background processes.)

3. To stop gracefully, send SIGINT (Ctrl+C) to each process.

## Notes
- The observer and resolver are independent; the resolver reads from the same database the observer writes to.
- If the resolver cannot connect to the RPC endpoint, it will log an error and retry on its next tick (every 5 seconds) but will not block the observer.
- Dissemination is rate-limited to 10 msgs/sec to avoid network abuse; this is within typical Ethereum client behavior.