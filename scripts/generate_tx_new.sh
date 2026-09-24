#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
RESULTS_DIR="$PROJECT_DIR/results"

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

# Function to send transaction from one node to another
send_transaction() {
    local from_node=$1
    local to_node=$2
    local from_account=$3
    local to_account=$4
    local amount_wei=$5
    local tx_description=$6

    echo "Sending transaction: $tx_description"
    echo "From: $from_account (node$from_node) -> To: $to_account (node$to_node)"
    echo "Amount: $amount_wei wei"

    # Unlock the sending account on the source node using HTTP RPC
    echo "personal.unlockAccount('$from_account', '', 0)" | docker compose exec "node${from_node}" geth attach http://localhost:8545 >/dev/null 2>&1

    # Send transaction
    local tx_hash=$(echo "eth.sendTransaction({from: '$from_account', to: '$to_account', value: web3.toWei('$amount_wei', 'wei')})" | docker compose exec "node${from_node}" geth attach http://localhost:8545 2>/dev/null)

    # Clean up the tx hash (remove quotes and extra characters)
    tx_hash=$(echo "$tx_hash" | tr -d '"' | tr -d '\r' | sed 's/^0x//' | sed 's/0x$//')

    if [[ "$tx_hash" =~ ^[0-9a-f]{64}$ ]]; then
        echo "Transaction sent successfully! Hash: 0x$tx_hash"

        # Wait for transaction to be mined (simple sleep approach)
        echo "Waiting for transaction to be mined..."
        sleep 5

        # Check transaction receipt
        local receipt=$(echo "eth.getTransactionReceipt('0x$tx_hash')" | docker compose exec "node${from_node}" geth attach http://localhost:8545 2>/dev/null)

        if [[ "$receipt" != "null" && "$receipt" != "" ]]; then
            echo "Transaction mined! Receipt: $receipt"
        else
            echo "Transaction submitted but not yet mined (check pool status)"
        fi

        # Return transaction hash for logging
        echo "0x$tx_hash"
    else
        echo "Failed to send transaction. Response: $tx_hash"
        return 1
    fi
}

# Function to check account balances
check_balances() {
    local node=$1
    shift
    local accounts=("$@")

    echo "=== Balances on node$node ==="
    for account in "${accounts[@]}"; do
        local balance=$(echo "web3.fromWei(eth.getBalance('$account'), 'ether')" | docker compose exec "node${node}" geth attach http://localhost:8545 2>/dev/null)
        echo "  $account: $balance ETH"
    done
}

# Main execution
echo "=== Ethereum Dropper Lab - Transaction Generation ==="
echo "Timestamp: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
echo ""

# Define accounts (from genesis allocation)
ACCOUNT_0="0x0000000000000000000000000000000000000000"  # Pre-funded in genesis
ACCOUNT_1="0xAbac9F99A31345dB7f388e6873E1CAFC25A2B8dc"
ACCOUNT_2="0xC210ab0eCD61F2566365b9DfaC53AF2d47373c90"
ACCOUNT_3="0xc5B66bbD749280E03060E83c95c1bbCC5A6205F1"
ACCOUNT_4="0xe1E2530C046Fecc728A4d36905A5a7ff41F936E7"

# Show initial balances
echo "Initial Balances:"
check_balances 1 $ACCOUNT_0 $ACCOUNT_1 $ACCOUNT_2 $ACCOUNT_3 $ACCOUNT_4
echo ""

# Send transactions between accounts on different nodes
echo "=== Sending Transactions ==="

# Transaction 1: node1 account -> node2 account (0.1 ETH)
TX1_HASH=$(send_transaction 1 2 $ACCOUNT_1 $ACCOUNT_2 "0.1" "Node1->Node2: 0.1 ETH")
echo ""

# Transaction 2: node2 account -> node3 account (0.05 ETH)
TX2_HASH=$(send_transaction 2 3 $ACCOUNT_2 $ACCOUNT_3 "0.05" "Node2->Node3: 0.05 ETH")
echo ""

# Transaction 3: node3 account -> node4 account (0.02 ETH)
TX3_HASH=$(send_transaction 3 4 $ACCOUNT_3 $ACCOUNT_4 "0.02" "Node3->Node4: 0.02 ETH")
echo ""

# Transaction 4: node4 account -> node1 account (0.01 ETH)
TX4_HASH=$(send_transaction 4 1 $ACCOUNT_4 $ACCOUNT_1 "0.01" "Node4->Node1: 0.01 ETH")
echo ""

# Show final balances
echo "Final Balances:"
check_balances 1 $ACCOUNT_0 $ACCOUNT_1 $ACCOUNT_2 $ACCOUNT_3 $ACCOUNT_4
echo ""

# Save transaction hashes to file
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
TX_LOG_FILE="$RESULTS_DIR/tx_hashes_${TIMESTAMP}.log"
{
    echo "Transaction Log - $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    echo "========================================"
    echo "TX1 (Node1->Node2): $TX1_HASH"
    echo "TX2 (Node2->Node3): $TX2_HASH"
    echo "TX3 (Node3->Node4): $TX3_HASH"
    echo "TX4 (Node4->Node1): $TX4_HASH"
} > "$TX_LOG_FILE"

echo "Transaction hashes saved to: $TX_LOG_FILE"
echo "=== Transaction Generation Complete ==="