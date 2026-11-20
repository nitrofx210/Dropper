package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	_ "modernc.org/sqlite"

	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

func main() {
	rpcEndpoint := flag.String("rpc", "", "RPC endpoint (required)")
	outputDb := flag.String("output", "rescan_batch.db", "Output database path")
	startBlock := flag.Int64("start", 26379065, "Start block number")
	endBlock := flag.Int64("end", 26379084, "End block number")
	useFinalizedTag := flag.Bool("finalized-tag", true, "Use finalized tag for validation")
	batchSize := flag.Int("batch-size", 20, "Batch size")
	sourceDb := flag.String("source-db", "D:/dropper-backup/analysis/mainnet_2026-11-05_analysis.db", "Source database with tracked transactions")
	flag.Parse()

	if *rpcEndpoint == "" {
		fmt.Fprintf(os.Stderr, "Error: -rpc is required\n")
		flag.Usage()
		os.Exit(1)
	}

	ctx := context.Background()

	// Open source database (read-only)
	srcDbURI := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", *sourceDb)
	srcDb, err := sql.Open("sqlite", srcDbURI)
	if err != nil {
		log.Fatalf("Failed to open source database: %v", err)
	}
	defer srcDb.Close()

	// Load tracked transaction hashes from source
	fmt.Println("Loading tracked transaction hashes from source...")
	rows, err := srcDb.Query(`SELECT tx_hash FROM tracked_transactions`)
	if err != nil {
		log.Fatalf("Failed to query tracked transactions: %v", err)
	}

	trackedHashes := make(map[string][]byte)
	for rows.Next() {
		var txHash []byte
		rows.Scan(&txHash)
		trackedHashes[hex.EncodeToString(txHash)] = txHash
	}
	rows.Close()
	fmt.Printf("Loaded %d tracked transaction hashes\n", len(trackedHashes))

	// Create output database
	if err := os.Remove(*outputDb); err != nil && !os.IsNotExist(err) {
		log.Fatalf("Failed to remove existing output db: %v", err)
	}

	sto, err := storage.New(*outputDb)
	if err != nil {
		log.Fatalf("Failed to create output database: %v", err)
	}
	defer sto.Close()

	// Connect to RPC
	client, err := ethclient.Dial(*rpcEndpoint)
	if err != nil {
		log.Fatalf("Failed to dial RPC: %v", err)
	}
	defer client.Close()

	// Create resolver run
	runID, err := sto.CreateResolverRun("observer-001", "go-ethereum/v1.17.7", "go1.26.5", "rescan_batch", "1", fmt.Sprintf("re-scan batch %d-%d", *startBlock, *endBlock))
	if err != nil {
		log.Fatalf("Failed to create resolver run: %v", err)
	}
	fmt.Printf("Created resolver run %d\n", runID)

	// Process blocks in batches
	currentBlock := *startBlock
	totalProcessed := 0
	totalMatches := 0
	startTime := time.Now()

	for currentBlock <= *endBlock {
		batchEnd := currentBlock + int64(*batchSize) - 1
		if batchEnd > *endBlock {
			batchEnd = *endBlock
		}

		// Fetch finalized head once per batch if using finalized tag
		var finalizedHead *types.Header
		if *useFinalizedTag {
			var rawHeader types.Header
			err := client.Client().CallContext(ctx, &rawHeader, "eth_getBlockByNumber", "finalized", false)
			if err == nil {
				finalizedHead = &rawHeader
				fmt.Printf("Batch %d-%d: finalized head = %d\n", currentBlock, batchEnd, finalizedHead.Number.Uint64())
			} else {
				log.Printf("Warning: failed to fetch finalized head: %v", err)
			}
		}

		// Process each block in batch
		for blockNum := currentBlock; blockNum <= batchEnd; blockNum++ {
			block, err := client.BlockByNumber(ctx, big.NewInt(blockNum))
			if err != nil {
				log.Printf("Failed to fetch block %d: %v", blockNum, err)
				continue
			}
			if block == nil {
				log.Printf("Got nil block for %d", blockNum)
				continue
			}

			// Validate block number matches request
			if block.NumberU64() != uint64(blockNum) {
				log.Printf("Block number mismatch: requested %d, got %d", blockNum, block.NumberU64())
				continue
			}

			// Validate against finalized head if available
			if finalizedHead != nil && block.NumberU64() > finalizedHead.Number.Uint64() {
				log.Printf("Block %d not yet finalized (head=%d)", blockNum, finalizedHead.Number.Uint64())
			}

			// Process block for tracked transactions
			matches := processBlock(block, trackedHashes, sto, runID)
			totalMatches += matches
			totalProcessed++

			// Special output for block 26379084
			if blockNum == 26379084 {
				fmt.Printf("\n=== Block 26379084 Details ===\n")
				fmt.Printf("  Block number: %d\n", block.NumberU64())
				fmt.Printf("  Block hash: %s\n", block.Hash().Hex())
				fmt.Printf("  Timestamp: %d (%s)\n", block.Time(), time.Unix(int64(block.Time()), 0).UTC().Format(time.RFC3339))
				fmt.Printf("  Total transactions: %d\n", len(block.Transactions()))
				fmt.Printf("  Matched tracked transactions: %d\n", matches)

				// Show matched transaction hashes
				for _, tx := range block.Transactions() {
					key := hex.EncodeToString(tx.Hash().Bytes())
					if _, ok := trackedHashes[key]; ok {
						fmt.Printf("    Match: %s\n", key)
					}
				}
			}
		}

		currentBlock = batchEnd + 1

		elapsed := time.Since(startTime)
		fmt.Printf("Progress: %d/%d blocks, %d matches, elapsed: %v\n",
			totalProcessed, *endBlock-*startBlock+1, totalMatches, elapsed.Round(time.Second))
	}

	elapsed := time.Since(startTime)
	fmt.Printf("\nBatch complete: %d blocks processed, %d matches found, elapsed: %v\n",
		totalProcessed, totalMatches, elapsed.Round(time.Second))

	// Persist final counts
	sto.UpdateResolverRunEnd(runID, time.Now().UnixNano())
	fmt.Printf("Results written to %s\n", *outputDb)
}

func processBlock(block *types.Block, trackedHashes map[string][]byte, sto *storage.Storage, runID int64) int {
	_ = block.Time() // blockTimestamp - used indirectly via blockTimeNs
	blockNumber := block.NumberU64()
	blockHash := block.Hash().Bytes()

	matches := 0

	// Get all transaction hashes in this block
	txHashes := make([][]byte, 0, len(block.Transactions()))
	for _, tx := range block.Transactions() {
		txHashes = append(txHashes, tx.Hash().Bytes())
	}

	if len(txHashes) == 0 {
		return 0
	}

	// Check each transaction in the block against our tracked transactions
	blockTimeNs := int64(block.Time()) * int64(time.Second)
	for i, txHash := range txHashes {
		key := hex.EncodeToString(txHash)
		if _, ok := trackedHashes[key]; ok {
			// Found a tracked transaction in this block
			firstSeenBeforeBlock := true // We don't have first_seen_ns here, assume true for stats

			if err := storeInclusionResult(sto, txHash, runID, blockNumber, blockHash, int64(i), blockTimeNs, time.Now().UnixNano(), firstSeenBeforeBlock); err != nil {
				log.Printf("Failed to store inclusion result for %s: %v", key, err)
				continue
			}
			matches++
		}
	}

	return matches
}

func storeInclusionResult(sto *storage.Storage, txHash []byte, runID int64, blockNumber uint64, blockHash []byte, txIndex int64, inclusionTimeNs int64, finalizedCheckedAtNs int64, deliveredBeforeBlock bool) error {
	const halfLifeSeconds = int64(780)
	finalizationEstimateNs := inclusionTimeNs + (halfLifeSeconds * int64(time.Second))

	var includedInt int64 = 0
	if deliveredBeforeBlock {
		includedInt = 1
	}

	_, err := sto.DB().Exec(`
		INSERT INTO transaction_inclusion (
			tx_hash, resolve_run_id, block_number, block_hash,
			inclusion_time_ns, finalized_checked_at_ns, finalization_estimate_ns,
			lookup_source, last_lookup_ns, lookup_attempts, delivered_before_block
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		txHash,
		runID,
		blockNumber,
		blockHash,
		inclusionTimeNs,
		finalizedCheckedAtNs,
		finalizationEstimateNs,
		"rpc_block",
		finalizedCheckedAtNs,
		1,
		includedInt,
	)

	return err
}