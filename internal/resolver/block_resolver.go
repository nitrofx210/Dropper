package resolver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// BlockResolver resolves transaction inclusion using block-based fetching
type BlockResolver struct {
	storage              *storage.Storage
	rpcClient            *ethclient.Client
	batchSize            int
	maxConcurrentRequests int
	postRunMarginSec     int64
	useFinalizedTag      bool
	confirmationDepth    int64
	resolverRunID        int64 // per-execution resolver run id (point 2: separate from collection run id)
}


// NewBlockResolver creates a new BlockResolver instance
func NewBlockResolver(sto *storage.Storage, rpcEndpoint string, batchSize int, maxConcurrentRequests int, postRunMarginSec int64) (*BlockResolver, error) {
	if rpcEndpoint == "" {
		return nil, ErrEmptyRPCEndpoint
	}

	// Default values
	if batchSize <= 0 {
		batchSize = 20
	}
	if maxConcurrentRequests <= 0 {
		maxConcurrentRequests = 5
	}
	if postRunMarginSec <= 0 {
		postRunMarginSec = 3600 // 1 hour
	}

	// Dial the RPC endpoint
	client, err := ethclient.Dial(rpcEndpoint)
	if err != nil {
		return nil, err
	}

	return &BlockResolver{
		storage:              sto,
		rpcClient:            client,
		batchSize:            batchSize,
		maxConcurrentRequests: maxConcurrentRequests,
		postRunMarginSec:     postRunMarginSec,
		useFinalizedTag:      true, // Default to using finalized tag as per design
		confirmationDepth:    64,   // Default confirmation depth (2 epochs)
	}, nil
}

// Close closes the RPC client
func (r *BlockResolver) Close() {
	r.rpcClient.Close()
}

// Run starts the block resolver loop
func (r *BlockResolver) Run(ctx context.Context) error {
	// Resolve the resolver run id used to label append-only inclusion rows (point 2).
	// Falls back to 0 when no resolver run has been recorded yet.
	lastRunID, err := r.storage.GetLastResolverRunID()
	if err != nil {
		return err
	}
	r.resolverRunID = lastRunID

	startTime, endTime, err := r.storage.GetLastRunTimes()
	if err != nil {
		return err
	}
	log.Info("resolver run times resolved", "start_ns", startTime, "end_ns", endTime,
		"start_sec", startTime/int64(time.Second), "end_sec", endTime/int64(time.Second),
		"span_s", (endTime-startTime)/int64(time.Second))

	// Get the last processed block from checkpoint (if any)
	lastProcessedBlock, err := r.getCheckpointBlock(ctx)
	if err != nil {
		return err
	}

	// Determine block range to process
	startBlock, endBlock, err := r.determineBlockRange(ctx, lastProcessedBlock)
	if err != nil {
		return err
	}
	log.Info("resolver block range resolved", "start_block", startBlock, "end_block", endBlock,
		"blocks", endBlock-startBlock+1)

	// Empty range: nothing to process — exit quickly rather than silently returning RC=0.
	if startBlock == 0 && endBlock == 0 {
		log.Warn("resolver empty block range: no blocks to process", "start_block", startBlock, "end_block", endBlock)
		return nil
	}

	// Process blocks in batches
	blocksFetched, err := r.processBlockRange(ctx, startBlock, endBlock)
	if err != nil {
		return err
	}
	log.Info("resolver block range processed", "blocks_fetched", blocksFetched,
		"start_block", startBlock, "end_block", endBlock)

	// Update checkpoint
	if err := r.updateCheckpoint(ctx, endBlock); err != nil {
		return err
	}

	return nil
}

// getCheckpointBlock retrieves the last successfully processed block number
func (r *BlockResolver) getCheckpointBlock(ctx context.Context) (int64, error) {
	// For now, we'll start from beginning - in a full implementation this would read from a checkpoint file
	// TODO: Implement proper checkpointing to disk
	return 0, nil
}

// preRunMarginSec is how far before the run's start time we begin scanning blocks
// (point 5: start a few minutes before run start to avoid missing edge transactions)
const preRunMarginSec = 300 // 5 minutes

// determineBlockRange calculates the start and end blocks to process based on collection run times.
// Uses binary search on block timestamps instead of a fixed 12s-per-block estimate (point 5).
func (r *BlockResolver) determineBlockRange(ctx context.Context, lastProcessedBlock int64) (int64, int64, error) {
	startTime, endTime, err := r.storage.GetLastRunTimes()
	if err != nil {
		return 0, 0, err
	}

	if startTime == 0 && endTime == 0 {
		return 0, 0, nil // No runs yet
	}

	// Convert nanoseconds → seconds for RPC comparison
	startSeconds := startTime / int64(time.Second)
	endSeconds := endTime / int64(time.Second)

	// Start a few minutes before the run's actual start (point 5)
	startSearchSec := startSeconds - preRunMarginSec
	endSearchSec := endSeconds + r.postRunMarginSec

	startBlock, err := r.findBlockAtTime(ctx, startSearchSec)
	if err != nil {
		return 0, 0, err
	}
	endBlock, err := r.findBlockAtTime(ctx, endSearchSec)
	if err != nil {
		return 0, 0, err
	}

	// Ensure we don't process blocks we've already processed
	if lastProcessedBlock >= startBlock {
		startBlock = lastProcessedBlock + 1
	}

	// Validate range
	if endBlock < startBlock {
		return 0, 0, nil // Nothing to process
	}

	return startBlock, endBlock, nil
}

// findBlockAtTime binary-searches block headers to find the first block whose
// timestamp is >= targetSec (Unix seconds). Returns head block if no block
// reaches the target (e.g. target is in the future).
func (r *BlockResolver) findBlockAtTime(ctx context.Context, targetSec int64) (int64, error) {
	head, err := r.rpcClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get head block: %w", err)
	}
	headNum := head.Number.Int64()

	lo := int64(0)
	hi := headNum
	ans := headNum // default to head if target is beyond all known blocks

	for lo <= hi {
		mid := (lo + hi) / 2
		ts, err := r.fetchBlockHeaderTimestamp(ctx, mid)
		if err != nil {
			// Block may not exist yet or fetch failed; search lower
			if mid == 0 {
				return 0, nil
			}
			hi = mid - 1
			continue
		}
		if int64(ts) >= targetSec {
			ans = mid
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}

	return ans, nil
}

// fetchBlockHeaderTimestamp returns just the block header timestamp (Unix seconds)
// for a given block number — cheaper than fetching a full block.
func (r *BlockResolver) fetchBlockHeaderTimestamp(ctx context.Context, blockNum int64) (uint64, error) {
	header, err := r.rpcClient.HeaderByNumber(ctx, big.NewInt(blockNum))
	if err != nil {
		return 0, err
	}
	return header.Time, nil
}

// processBlockRange processes blocks from start to end (inclusive) using batching.
// Returns the number of blocks successfully fetched from RPC.
func (r *BlockResolver) processBlockRange(ctx context.Context, startBlock int64, endBlock int64) (int, error) {
	if startBlock > endBlock {
		return 0, nil
	}

	blocksFetched := 0
	// Process blocks in batches
	currentBlock := startBlock
	for currentBlock <= endBlock {
		// Calculate batch end
		batchEnd := currentBlock + int64(r.batchSize) - 1
		if batchEnd > endBlock {
			batchEnd = endBlock
		}

		// Process this batch
		batchFetched, err := r.processBlockBatch(ctx, currentBlock, batchEnd)
		if err != nil {
			return 0, err
		}
		blocksFetched += batchFetched

		// Move to next batch
		currentBlock = batchEnd + 1

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return blocksFetched, ctx.Err()
		default:
			// Continue processing
		}
	}

	return blocksFetched, nil
}

// processBlockBatch processes a batch of block numbers. Returns the number of
// blocks successfully fetched from RPC.
func (r *BlockResolver) processBlockBatch(ctx context.Context, startBlock int64, endBlock int64) (int, error) {
	blockNumbers := make([]int64, 0, endBlock-startBlock+1)
	for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
		blockNumbers = append(blockNumbers, blockNum)
	}

	fetched := 0
	for _, blockNum := range blockNumbers {
		// Check context
		select {
		case <-ctx.Done():
			return fetched, ctx.Err()
		default:
			// Continue
		}

		log.Debug("resolver fetching block", "block_number", blockNum)

		var params []interface{}
		if r.useFinalizedTag && blockNum == endBlock {
			params = []interface{}{"finalized", false}
		} else {
			params = []interface{}{fmt.Sprintf("0x%x", blockNum), false}
		}

		block, err := r.rpcClient.BlockByNumber(ctx, rpcBlockNumberFromParams(params))
		if err != nil {
			log.Warn("resolver failed to fetch block", "block_number", blockNum, "err", err)
			continue
		}

		// Process the block
		if err := r.processBlock(ctx, block); err != nil {
			log.Warn("resolver failed to process block", "block_number", blockNum, "err", err)
			continue
		}
		fetched++
	}

	log.Info("resolver batch processed", "start_block", startBlock, "end_block", endBlock,
		"blocks_fetched", fetched, "blocks_requested", len(blockNumbers))

	return fetched, nil
}

// rpcBlockNumberFromParams extracts block number from params for BlockByNumber call
func rpcBlockNumberFromParams(params []interface{}) *big.Int {
	if len(params) < 1 {
		return nil
	}

	switch val := params[0].(type) {
	case string:
		if len(val) >= 2 && val[:2] == "0x" {
			if num, err := hexToBigInt(val[2:]); err == nil {
				return num
			}
		}
	case *big.Int:
		return val
	}
	return nil
}

// hexToBigInt converts hex string to big.Int
func hexToBigInt(s string) (*big.Int, error) {
	if len(s) == 0 {
		return big.NewInt(0), nil
	}
	num := new(big.Int)
	if _, ok := num.SetString(s, 16); !ok {
		return nil, fmt.Errorf("invalid hex string: %s", s)
	}
	return num, nil
}

// processBlock processes a single block for transaction inclusion
func (r *BlockResolver) processBlock(ctx context.Context, block *types.Block) error {
	if block == nil {
		return nil
	}

	blockTimestamp := block.Time()
	blockNumber := block.NumberU64()
	blockHash := block.Hash().Bytes()

	log.Debug("Processing block", "number", blockNumber, "timestamp", blockTimestamp, "tx_count", len(block.Transactions()))

	// Get all transaction hashes in this block
	txHashes := make([][]byte, 0, len(block.Transactions()))
	for _, tx := range block.Transactions() {
		txHashes = append(txHashes, tx.Hash().Bytes())
	}

	// Look up transaction hashes in the database using the tx_hash index
	// Build a query to find which of these hashes exist in tracked_transactions
	if len(txHashes) == 0 {
		return nil
	}

	// Create placeholders for the IN clause
	placeholders := make([]string, len(txHashes))
	args := make([]interface{}, len(txHashes))
	for i, txHash := range txHashes {
		placeholders[i] = "?"
		args[i] = txHash
	}

	// Query the database for matching transactions
	query := fmt.Sprintf(`
		SELECT tx_hash, first_seen_ns, last_checked_ns, next_check_ns, attempt, included, block_number, block_hash, transaction_index, useful_score
		FROM tracked_transactions
		WHERE tx_hash IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := r.storage.DB().Query(query, args...)
	if err != nil {
		return fmt.Errorf("failed to query tracked transactions: %w", err)
	}
	defer rows.Close()

	// Create a map for fast lookup of matching transactions
	txHashMap := make(map[string]*storage.TrackedTransaction)
	for rows.Next() {
		var txHash []byte
		var firstSeenNs, lastCheckedNs, nextCheckNs, attempt int64
		var includedInt sql.NullInt64 // 0 = false, 1 = true, NULL = unknown
		var blockNumber sql.NullInt64
		var blockHash []byte
		var transactionIndex sql.NullInt64
		var usefulScore sql.NullFloat64
		err := rows.Scan(&txHash, &firstSeenNs, &lastCheckedNs, &nextCheckNs, &attempt, &includedInt, &blockNumber, &blockHash, &transactionIndex, &usefulScore)
		if err != nil {
			return fmt.Errorf("failed to scan tracked transaction: %w", err)
		}
		var included *bool
		if includedInt.Valid {
			b := includedInt.Int64 == 1
			included = &b
		}
		var blockNumberPtr *int64
		if blockNumber.Valid {
			blockNumberPtr = &blockNumber.Int64
		}
		var transactionIndexPtr *int64
		if transactionIndex.Valid {
			transactionIndexPtr = &transactionIndex.Int64
		}
		var usefulScorePtr *float64
		if usefulScore.Valid {
			usefulScorePtr = &usefulScore.Float64
		}
		txHashMap[hex.EncodeToString(txHash)] = &storage.TrackedTransaction{
			TxHash:          txHash,
			FirstSeenNs:     firstSeenNs,
			LastCheckedNs:   lastCheckedNs,
			NextCheckNs:     nextCheckNs,
			Attempt:         int(attempt),
			Included:        included,
			BlockNumber:     blockNumberPtr,
			BlockHash:       blockHash,
			TransactionIndex:transactionIndexPtr,
			UsefulScore:     usefulScorePtr,
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("error iterating tracked transactions: %w", err)
	}

	log.Debug("Found tracked transactions in block", "count", len(txHashMap))

	// Check each transaction in the block against our tracked transactions.
// Point 4: inclusion facts (block number, hash, timestamp) are stored
// UNCONDITIONALLY for every tracked transaction found in a block.
// Point 4: "first_seen < block time" is a SEPARATE derived flag (recorded as
// inclusion_flag), not part of the inclusion storage decision.
// Point 6: the resolver never writes to tracked_transactions or any observation
// table — it only writes to transaction_inclusion.
for i, txHash := range txHashes {
    txHashHex := hex.EncodeToString(txHash)
    if trackedTx, exists := txHashMap[txHashHex]; exists {
        blockTimeNs := int64(block.Time()) * int64(time.Second)

        // Derived flag: was the transaction first seen before this block's time?
        // This is separate from the inclusion storage decision itself.
        firstSeenBeforeBlock := trackedTx.FirstSeenNs < blockTimeNs

        // Store inclusion fact unconditionally (points 4 & 6).
        if err := r.storeInclusionResult(
            trackedTx.TxHash,
            r.resolverRunID,
            blockNumber,
            blockHash,
            int64(i),
            blockTimeNs,
            time.Now().UnixNano(),
            firstSeenBeforeBlock,
        ); err != nil {
            return err
        }

        log.Debug("Stored inclusion fact", "tx_hash", txHashHex, "block_number", blockNumber,
            "first_seen_before_block", firstSeenBeforeBlock)
    }
    // If transaction not in our tracked set, we ignore it (not our concern).
}

return nil
}

// storeInclusionResult stores the inclusion result in the transaction_inclusion table.
// Point 4: append-only — uses plain INSERT (never INSERT OR REPLACE) so each call
// writes a new row keyed by (tx_hash, resolve_run_id) and never overwrites earlier results.
// Point 6: writes only to transaction_inclusion, never to tracked_transactions or observation tables.
// The deliveredBeforeBlock parameter records the derived "first_seen < block time" flag
// (separate from the unconditional inclusion fact storage, point 4).
func (r *BlockResolver) storeInclusionResult(txHash []byte, runID int64, blockNumber uint64, blockHash []byte, txIndex int64, inclusionTimeNs int64, finalizedCheckedAtNs int64, deliveredBeforeBlock bool) error {
    // Calculate derived finalization estimate (block timestamp + ~13 minutes)
    const halfLifeSeconds = int64(780) // ~13 minutes
    finalizationEstimateNs := inclusionTimeNs + (halfLifeSeconds * int64(time.Second))

    var includedInt int64 = 0
    if deliveredBeforeBlock {
        includedInt = 1
    }

    _, err := r.storage.DB().Exec(`
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

// updateCheckpoint updates the last processed block checkpoint
func (r *BlockResolver) updateCheckpoint(ctx context.Context, lastProcessedBlock int64) error {
	// TODO: Implement proper checkpointing to disk or database
	// For now, this is a placeholder
	return nil
}

// sqlBoolPtr creates a sql.NullBool from a bool pointer
func sqlBoolPtr(b *bool) sql.NullBool {
	if b == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: *b, Valid: true}
}

// sqlInt64Ptr creates a sql.NullInt64 from an int64 pointer
func sqlInt64Ptr(i *int64) sql.NullInt64 {
	if i == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *i, Valid: true}
}

// processBlockRangeAlternative is an alternative implementation that processes blocks individually
// This is kept for reference but the batched version above is preferred
func (r *BlockResolver) processBlockRangeAlternative(ctx context.Context, startBlock int64, endBlock int64) error {
	for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
		// Check context
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue
		}

		// Get block
		var params []interface{}
		if r.useFinalizedTag && blockNum == endBlock {
			params = []interface{}{"finalized", false}
		} else {
			params = []interface{}{fmt.Sprintf("0x%x", blockNum), false}
		}

		block, err := r.rpcClient.BlockByNumber(ctx, rpcBlockNumberFromParams(params))
		if err != nil {
			continue
		}

		// Process block
		if err := r.processBlock(ctx, block); err != nil {
			continue
		}
	}

	return nil
}