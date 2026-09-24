# Verification Plan for Ethereum Dropper Research Project

## Phase 1: Environment Stabilization
### Goal: Establish working 4-node Geth network with Docker connectivity

### Tasks:
1. Fix Docker path issues in scripts
2. Wait for Docker daemon to be ready before starting containers
3. Start network and verify basic connectivity

### Success Criteria:
- All 4 nodes start without errors
- `docker compose ps` shows all containers healthy
- Inter-node ping works (node1 can ping node2/node3/node4)
- Basic peer connectivity: `net.peerCount` shows >0 for each node

### Verification Commands:
```bash
./scripts/start.sh
docker compose ps  # Should show 4 healthy containers
docker compose exec node1 ping -c 3 node2  # Should succeed
docker compose exec node1 geth attach --exec "net.peerCount" ipc:/data/geth.ipc  # Should return >0
```

## Phase 2: Account & Transaction Foundation
### Goal: Create working accounts and establish baseline transaction capability

### Tasks:
1. Create accounts in each node's keystore matching genesis allocation
2. Verify accounts are readable and have correct balances from genesis
3. Establish baseline transaction capability

### Success Criteria:
- Accounts exist in each node's keystore
- Account addresses match genesis pre-funded allocations
- Balance queries return expected genesis amounts
- At least one transaction method works

### Verification Commands:
```bash
# Create accounts (example for node1)
docker run --rm -v ./nodes/node1:/data -v ./password.sec:/password.sec:ro --entrypoint="" ethereum/client-go:stable geth account new --datadir /data --password /password.sec

ls -la ./nodes/node1/keystore/  # Should show account files
./nodes/node1/keystore/*  # Should be valid account JSON
docker compose exec node1 geth account list --datadir /data  # Should show addresses
docker compose exec node1 geth attach --exec "eth.getBalance('0xAccountFromGenesis')" http://localhost:8545  # Should return non-zero balance
```

## Phase 3: Peer Event Instrumentation
### Goal: Monitor and record peer connection/disconnection events

### Tasks:
1. Create peer monitoring script that polls admin.peers and net.peerCount
2. Record connection/disconnection events with timestamps
3. Store peer metadata (enode, IP, port)
4. Output structured data to results/

### Success Criteria:
- Peer connection events recorded with timestamps when nodes start/connect
- Disconnection events recorded when nodes stop
- Peer metadata includes enode and address information
- Data stored in structured format (JSONL or CSV)

### Verification Commands:
```bash
# After starting network
./scripts/peer_monitor.sh &
# Stop one node
docker compose stop node2
sleep 2
docker compose start node2
# Check results/peer_events/ for connect/disconnect records with timestamps
cat results/peer_events/*.jsonl  # Should show events
```

## Phase 4: Transaction Reception Instrumentation
### Goal: Monitor and record transaction reception events

### Tasks:
1. Create transaction monitoring script that polls txpool
2. Capture new transaction hashes with reception timestamps
3. Record basic tx metadata (value, gas, etc.)
4. Output structured data to results/

### Success Criteria:
- Transaction hashes captured when transactions are sent
- Basic metadata (value, hash) recorded
- Events timestamped with reasonable precision
- No duplicate recording of same transaction

### Verification Commands:
```bash
./scripts/tx_monitor.sh &
./scripts/generate_tx.sh  # Generate test transaction
# Check results/transaction_events/ for new record
cat results/transaction_events/*.jsonl  # Should show transaction with hash and timestamp
```

## Phase 5: Blockchain Inclusion Tracking
### Goal: Track transactions from reception to blockchain inclusion

### Tasks:
1. Create inclusion tracker that monitors transaction hashes
2. Periodically check eth.getTransactionReceipt for each hash
3. Record inclusion status, block number, inclusion timestamp
4. Calculate latency from reception to inclusion
5. Output structured data to results/

### Success Criteria:
- Transactions move from "received" to "included" status
- Block numbers and inclusion timestamps recorded
- Latency calculations work correctly
- Orphaned/stuck transactions properly tracked

### Verification Commands:
```bash
./scripts/inclusion_tracker.sh &
./scripts/generate_tx.sh
# Wait for block inclusion (may need to mine block)
# Check results/transaction_outcomes/ for included=true record
cat results/transaction_outcomes/*.jsonl  # Should show transaction with inclusion data
```

## Phase 6: Controlled Validation Experiments
### Goal: Validate all instrumentation works correctly in controlled tests

### Tasks:
1. Test peer connection recording
2. Test transaction observation and recording  
3. Test transaction-to-block correlation
4. Test peer disconnection/reconnection event capture
5. Test sustained operation data consistency

### Success Criteria:
- All validation tests pass and produce expected data
- Experiments are repeatable and automated
- Data quality verified (no gaps, reasonable timestamps)

### Verification Commands:
```bash
# Run validation test suite
./scripts/test_connectivity.sh
./scripts/test_transaction_flow.sh
./scripts/test_peer_events.sh
./scripts/test_inclusion_tracking.sh
# All should return success
```

## Phase 7: Dropper Mechanism Implementation
### Goal: Implement and test Dropper mechanisms

### Tasks:
1. Implement random peer drop baseline
2. Expose usefulness metrics without committing to scoring formula
3. Ensure clean separation of data collection from scoring logic

### Success Criteria:
- Random peer drop mechanism works and is measurable
- Usefulness metrics expose meaningful differences between peers
- Implementation cleanly separates data collection from scoring logic
- Metrics are actionable for research purposes

### Verification Commands:
```bash
./scripts/dropper_baseline.sh
# Should select random peer, isolate it, measure effects
./scripts/usefulness_metrics.sh
# Should output raw metrics: tx count, inclusion ratio, latency, etc. per peer
```

## Phase 8: Mainnet Portability Documentation
### Goal: Document path to Mainnet deployment

### Tasks:
1. Document what changes are needed for Mainnet deployment
2. Confirm instrumentation works with public Geth node (conceptually)
3. Create deployment guide for migrating from local testnet to Mainnet

### Success Criteria:
- Documentation clear enough for independent reproduction
- Mainnet deployment path well-defined
- Minimal configuration changes required for production use
- Instructions specify what to change vs what to keep

### Verification Commands:
```bash
# Conceptual verification - would need actual Mainnet node
# docs/mainnet_deployment.md should specify:
# - Replace genesis with Mainnet checkpoint sync
# - Remove --nat flag, adjust port exposure if needed
# - Confirm peer monitoring scales with Mainnet diversity
# - Confirm transaction observation works with public txpool
```

## Immediate Next Steps

Given the environment issues, let's start with:

1. Fix the Docker path in start.sh
2. Try to start Docker Desktop manually if needed
3. Start the network and verify basic connectivity
4. Then proceed with account creation and instrumentation

Let me know when you'd like me to proceed with fixing the Docker issues and starting the network, and then we can work through each phase systematically.
