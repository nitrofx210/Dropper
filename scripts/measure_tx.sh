#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
RESULTS_DIR="$PROJECT_DIR/results"

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

# Function to measure transaction propagation
measure_transaction_propagation() {
    local from_node=$1
    local to_node=$2
    local from_account=$3
    local to_account=$4
    local amount_wei=$5
    local test_name=$6

    echo "Measuring propagation: $test_name"
    echo "From node$from_node ($from_account) -> node$to_node ($to_account)"
    echo "Amount: $amount_wei wei"

    # Record start time
    local start_time=$(date +%s.%N)

    # Unlock sending account using IPC
    docker compose exec "node${from_node}" geth attach --exec "personal.unlockAccount('$from_account', '', 0)" ipc:/data/geth.ipc >/dev/null 2>&1

    # Send transaction and capture hash
    local tx_hash=$(docker compose exec "node${from_node}" geth attach --exec "eth.sendTransaction({from: '$from_account', to: '$to_account', value: web3.toWei('$amount_wei', 'wei')})" ipc:/data/geth.ipc 2>/dev/null)

    # Clean up tx hash
    tx_hash=$(echo "$tx_hash" | tr -d '"' | tr -d '\r' | sed 's/^0x//' | sed 's/0x$//')

    if [[ ! "$tx_hash" =~ ^[0-9a-f]{64}$ ]]; then
        echo "Failed to send transaction: $tx_hash"
        return 1
    fi

    echo "Transaction sent. Hash: 0x$tx_hash"

    # Wait for transaction to appear in all nodes' mempools/blocks
    local propagated=false
    local check_interval=1  # seconds
    local max_wait=30  # maximum wait time in seconds
    local elapsed=0

    while [[ $elapsed -lt $max_wait && "$propagated" == false ]]; do
        sleep $check_interval
        elapsed=$((elapsed + check_interval))

        # Check if transaction is in mempool or mined on target node
        local tx_status=$(docker compose exec "node${to_node}" geth attach --exec "eth.getTransaction('0x$tx_hash')" ipc:/data/geth.ipc 2>/dev/null || echo "null")

        if [[ "$tx_status" != "null" && "$tx_status" != "" ]]; then
            propagated=true
            break
        fi

        # Also check if it's been mined (look for block number)
        local block_number=$(docker compose exec "node${to_node}" geth attach --exec "eth.getBlockTransactionCountByHash(eth.getBlock('latest').hash)" ipc:/data/geth.ipc 2>/dev/null)
    done

    # Record end time
    local end_time=$(date +%s.%N)
    local propagation_time=$(echo "$end_time - $start_time" | bc)

    if [[ "$propagated" == true ]]; then
        echo "Transaction propagated to node$to_node in ${propagation_time}s"

        # Check if transaction was mined
        local receipt=$(docker compose exec "node${from_node}" geth attach --exec "eth.getTransactionReceipt('0x$tx_hash')" ipc:/data/geth.ipc 2>/dev/null)

        local mined=false
        if [[ "$receipt" != "null" && "$receipt" != "" ]]; then
            # Check if receipt has a blockNumber (not null)
            local block_num=$(echo "$receipt" | grep -o '"blockNumber":"0x[0-9a-f]*"' | cut -d'"' -f4)
            if [[ "$block_num" != "0x0" && "$block_num" != "" ]]; then
                mined=true
            fi
        fi

        if [[ "$mined" == true ]]; then
            echo "Transaction mined on node$from_node"
        else
            echo "Transaction in mempool but not yet mined"
        fi

        # Return results
        echo "{\"success\":true,\"propagation_time\":$propagation_time,\"mined\":$mined,\"tx_hash\":\"0x$tx_hash\"}"
    else
        echo "Transaction did not propagate to node$to_node within ${max_wait}s"
        echo "{\"success\":false,\"propagation_time\":$propagation_time,\"mined\":false,\"tx_hash\":\"0x$tx_hash\"}"
    fi
}

# Function to measure network-wide propagation
measure_network_propagation() {
    local from_node=$1
    local from_account=$2
    local to_account=$3
    local amount_wei=$4
    local test_name=$5

    echo "Measuring network-wide propagation: $test_name"
    echo "From node$from_node ($from_account) -> $to_account (broadcast)"
    echo "Amount: $amount_wei wei"

    # Record start time
    local start_time=$(date +%s.%N)

    # Unlock sending account
    docker compose exec "node${from_node}" geth attach --exec "personal.unlockAccount('$from_account', '', 0)" ipc:/data/geth.ipc >/dev/null 2>&1

    # Send transaction
    local tx_hash=$(docker compose exec "node${from_node}" geth attach --exec "eth.sendTransaction({from: '$from_account', to: '$to_account', value: web3.toWei('$amount_wei', 'wei')})" ipc:/data/geth.ipc 2>/dev/null)

    # Clean up tx hash
    tx_hash=$(echo "$tx_hash" | tr -d '"' | tr -d '\r' | sed 's/^0x//' | sed 's/0x$//')

    if [[ ! "$tx_hash" =~ ^[0-9a-f]{64}$ ]]; then
        echo "Failed to send transaction: $tx_hash"
        return 1
    fi

    echo "Transaction sent. Hash: 0x$tx_hash"
    echo "Checking propagation to all nodes..."

    # Check each node
    local results=()
    local all_propagated=true

    for target_node in {1..4}; do
        local node_start=$(date +%s.%N)
        local propagated=false
        local check_interval=1
        local max_wait=20
        local elapsed=0

        while [[ $elapsed -lt $max_wait && "$propagated" == false ]]; do
            sleep $check_interval
            elapsed=$((elapsed + check_interval))

            local tx_status=$(docker compose exec "node${target_node}" geth attach --exec "eth.getTransaction('0x$tx_hash')" ipc:/data/geth.ipc 2>/dev/null || echo "null")

            if [[ "$tx_status" != "null" && "$tx_status" != "" ]]; then
                propagated=true
                local node_end=$(date +%s.%N)
                local node_prop_time=$(echo "$node_end - $node_start" | bc)
                results+=("node${target_node}:${node_prop_time}")
                break
            fi
        done

        if [[ "$propagated" == false ]]; then
            results+=("node${target_node}:timeout")
            all_propagated=false
        fi
    done

    # Record end time
    local end_time=$(date +%s.%N)
    local total_time=$(echo "$end_time - $start_time" | bc)

    echo "Propagation results:"
    for result in "${results[@]}"; do
        local node=$(echo "$result" | cut -d':' -f1)
        local time=$(echo "$result" | cut -d':' -f2)
        if [[ "$time" == "timeout" ]]; then
            echo "  $node: TIMEOUT (>20s)"
        else
            echo "  $node: ${time}s"
        fi
    done

    # Check if transaction was mined
    local receipt=$(docker compose exec "node${from_node}" geth attach --exec "eth.getTransactionReceipt('0x$tx_hash')" ipc:/data/geth.ipc 2>/dev/null)

    local mined=false
    if [[ "$receipt" != "null" && "$receipt" != "" ]]; then
        local block_num=$(echo "$receipt" | grep -o '"blockNumber":"0x[0-9a-f]*"' | cut -d'"' -f4)
        if [[ "$block_num" != "0x0" && "$block_num" != "" ]]; then
            mined=true
        fi
    fi

    echo "Transaction mined: $mined"
    echo "Total measurement time: ${total_time}s"

    # Return results
    echo "{\"success\":$all_propagated,\"total_time\":$total_time,\"mined\":$mined,\"tx_hash\":\"0x$tx_hash\",\"details\":[${results[@]}]}"
}

# Main execution
echo "=== Ethereum Dropper Lab - Transaction Measurement ==="
echo "Timestamp: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
echo ""

# Define accounts
ACCOUNT_0="0x0000000000000000000000000000000000000000"
ACCOUNT_1="0xAbac9F99A31345dB7f388e6873E1CAFC25A2B8dc"
ACCOUNT_2="0xC210ab0eCD61F2566365b9DfaC53AF2d47373c90"
ACCOUNT_3="0xc5B66bbD749280E03060E83c95c1bbCC5A6205F1"
ACCOUNT_4="0xe1E2530C046Fecc728A4d36905A5a7ff41F936E7"

echo "=== Running Propagation Tests ==="
echo ""

# Test 1: Measure propagation from node1 to node2
echo "Test 1: Node1 -> Node2 (0.5 ETH)"
RESULT1=$(measure_transaction_propagation 1 2 $ACCOUNT_1 $ACCOUNT_2 "0.5" "Node1->Node2")
echo "Result: $RESULT1"
echo ""

# Test 2: Measure propagation from node2 to node3
echo "Test 2: Node2 -> Node3 (0.25 ETH)"
RESULT2=$(measure_transaction_propagation 2 3 $ACCOUNT_2 $ACCOUNT_3 "0.25" "Node2->Node3")
echo "Result: $RESULT2"
echo ""

# Test 3: Measure propagation from node3 to node4
echo "Test 3: Node3 -> Node4 (0.1 ETH)"
RESULT3=$(measure_transaction_propagation 3 4 $ACCOUNT_3 $ACCOUNT_4 "0.1" "Node3->Node4")
echo "Result: $RESULT3"
echo ""

# Test 4: Measure network-wide broadcast from node1
echo "Test 4: Network-wide broadcast from Node1 (0.05 ETH)"
RESULT4=$(measure_network_propagation 1 $ACCOUNT_1 $ACCOUNT_4 "0.05" "Node1 Broadcast")
echo "Result: $RESULT4"
echo ""

# Save all results to file
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
MEASUREMENT_LOG_FILE="$RESULTS_DIR/tx_measurements_${TIMESTAMP}.json"
{
    echo "["
    echo "  {"
    echo "    \"test\": \"Node1->Node2 (0.5 ETH)\","
    echo "    \"timestamp\": \"$(date -u +"%Y-%m-%dT%H:%M:%SZ")\","
    echo "    \"result\": $RESULT1"
    echo "  },"
    echo "  {"
    echo "    \"test\": \"Node2->Node3 (0.25 ETH)\","
    echo "    \"timestamp\": \"$(date -u +"%Y-%m-%dT%H:%M:%SZ")\","
    echo "    \"result\": $RESULT2"
    echo "  },"
    echo "  {"
    echo "    \"test\": \"Node3->Node4 (0.1 ETH)\","
    echo "    \"timestamp\": \"$(date -u +"%Y-%m-%dT%H:%M:%SZ")\","
    echo "    \"result\": $RESULT3"
    echo "  },"
    echo "  {"
    echo "    \"test\": \"Node1 Broadcast (0.05 ETH)\","
    echo "    \"timestamp\": \"$(date -u +"%Y-%m-%dT%H:%M:%SZ")\","
    echo "    \"result\": $RESULT4"
    echo "  }"
    echo "]"
} > "$MEASUREMENT_LOG_FILE"

echo "Measurement results saved to: $MEASUREMENT_LOG_FILE"
echo "=== Transaction Measurement Complete ==="