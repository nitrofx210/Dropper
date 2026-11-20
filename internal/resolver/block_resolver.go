package resolver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// BlockResolver resolves transaction inclusion using block-based fetching
type BlockResolver struct {
	storage               *storage.Storage
	rpcClient             *ethclient.Client
	batchSize             int
	maxConcurrentRequests int
	postRunMarginSec      int64
	useFinalizedTag       bool
	confirmationDepth     int64
	resolverRunID         int64 // per-execution resolver run id (point 2: separate from collection run id)
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
		storage:               sto,
		rpcClient:             client,
		batchSize:             batchSize,
		maxConcurrentRequests: maxConcurrentRequests,
		postRunMarginSec:      postRunMarginSec,
		useFinalizedTag:       true, // Default to using finalized tag as per design
		confirmationDepth:     64,   // Default confirmation depth (2 epochs)
	}, nil
}

// Close closes the RPC client
func (r *BlockResolver) Close() {
	r.rpcClient.Close()
}

// Run starts the block resolver loop
func (r *BlockResolver) Run(ctx context.Context) error {
	// Get collection run times to determine block range
	startTime, endTime, err := r.storage.GetLastRunTimes()
	if err != nil {
		return err
	}
	if startTime == 0 && endTime == 0 {
		log.Warn("resolver empty block range: no collection runs found")
		return nil
	}
	log.Info("resolver run times resolved", "start_ns", startTime, "end_ns", endTime,
		"start_sec", startTime/int64(time.Second), "end_sec", endTime/int64(time.Second),
		"span_s", (endTime-startTime)/int64(time.Second))

	// Determine block range from collection run times (without checkpoint first)
	startBlock, endBlock, err := r.determineBlockRange(ctx)
	if err != nil {
		return err
	}
	log.Info("resolver block range resolved", "start_block", startBlock, "end_block", endBlock,
		"blocks", endBlock-startBlock+1)

	if startBlock == 0 && endBlock == 0 {
		log.Warn("resolver empty block range: no blocks to process", "start_block", startBlock, "end_block", endBlock)
		return nil
	}

	// Check for existing incomplete resolver run to resume
	lastRunID, err := r.storage.GetLastResolverRunID()
	if err != nil {
		return err
	}

	var resumeFromBlock int64 = startBlock
	if lastRunID > 0 {
		// Check if the last run has processed any blocks
		checkpoint, err := r.storage.GetResolverCheckpoint(lastRunID)
		if err != nil {
			return err
		}
		if checkpoint > 0 {
			// Resume from the next block after the last processed one
			resumeFromBlock = checkpoint + 1
			r.resolverRunID = lastRunID
			log.Info("resuming existing resolver run", "run_id", lastRunID, "resume_from_block", resumeFromBlock)
		}
	}

	// If no existing run to resume, create a new resolver run
	if r.resolverRunID == 0 {
		// Get config info from storage or use defaults
		observerID := "observer-001"
		gethLibVersion := "go-ethereum/v1.17.7"
		goBuildVersion := "go1.26.5"
		configHash := ""
		schemaVersion := "1"
		notes := "block resolver run"

		runID, err := r.storage.CreateResolverRun(observerID, gethLibVersion, goBuildVersion, configHash, schemaVersion, notes)
		if err != nil {
			return err
		}
		r.resolverRunID = runID
		log.Info("created new resolver run", "run_id", runID)
	}

	// Ensure we don't process before the determined start block
	if resumeFromBlock < startBlock {
		resumeFromBlock = startBlock
	}

	// Validate range
	if resumeFromBlock > endBlock {
		log.Info("resolver nothing to process: already up to date", "resume_from", resumeFromBlock, "end_block", endBlock)
		// Mark run as complete
		r.storage.UpdateResolverRunEnd(r.resolverRunID, time.Now().UnixNano())
		return nil
	}

	log.Info("resolver processing blocks", "start_block", resumeFromBlock, "end_block", endBlock)

	// Process blocks in batches
	blocksFetched, err := r.processBlockRange(ctx, resumeFromBlock, endBlock)
	if err != nil {
		return err
	}
	log.Info("resolver block range processed", "blocks_fetched", blocksFetched,
		"start_block", resumeFromBlock, "end_block", endBlock)

	// Mark resolver run as complete
	if err := r.storage.UpdateResolverRunEnd(r.resolverRunID, time.Now().UnixNano()); err != nil {
		return err
	}

	return nil
}

// preRunMarginSec is how far before the run's start time we begin scanning blocks
// (point 5: start a few minutes before run start to avoid missing edge transactions)
const preRunMarginSec = 300 // 5 minutes

// determineBlockRange calculates the start and end blocks to process based on collection run times.
// Uses binary search on block timestamps instead of a fixed 12s-per-block estimate (point 5).
func (r *BlockResolver) determineBlockRange(ctx context.Context) (int64, int64, error) {
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

	// If using finalized tag, fetch the finalized head once per batch to validate
	// that the blocks we're about to fetch are at or below the finalized block.
	var finalizedHead *types.Header
	if r.useFinalizedTag {
		var err error
		finalizedHead, err = r.rpcClient.HeaderByNumber(ctx, nil) // nil = latest, but we want finalized
		if err != nil {
			log.Warn("resolver failed to fetch finalized head", "err", err)
		} else {
			// HeaderByNumber with nil returns latest. To get finalized, we need a raw call.
			var rawHeader types.Header
			err = r.rpcClient.Client().CallContext(ctx, &rawHeader, "eth_getBlockByNumber", "finalized", false)
			if err == nil {
				finalizedHead = &rawHeader
			}
		}
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

		// Always fetch by explicit block number
		block, err := r.rpcClient.BlockByNumber(ctx, big.NewInt(blockNum))
		if err != nil {
			log.Warn("resolver failed to fetch block", "block_number", blockNum, "err", err)
			continue
		}
		if block == nil {
			log.Warn("resolver got nil block", "block_number", blockNum)
			continue
		}

		// Validate block number matches request
		if block.NumberU64() != uint64(blockNum) {
			log.Warn("resolver block number mismatch", "requested", blockNum, "got", block.NumberU64())
			continue
		}

		// If we have a finalized head, ensure the block is finalized (or we're OK with non-finalized for recent blocks)
		if finalizedHead != nil && block.NumberU64() > finalizedHead.Number.Uint64() {
			log.Debug("resolver block not yet finalized", "block_number", blockNum, "finalized_head", finalizedHead.Number.Uint64())
			// Don't skip - just log. The resolver processes all blocks in range.
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

	// Use a slice instead of map for memory efficiency (typical matches ~130/block).
	// Linear search is cache-friendly and avoids map overhead (key strings, pointers).
	type trackedMatch struct {
		txHash           []byte
		firstSeenNs      int64
		included         *bool
		blockNumberPtr   *int64
		transactionIndex *int64
	}
	matches := make([]trackedMatch, 0, 256)
	for rows.Next() {
		var txHash []byte
		var firstSeenNs, lastCheckedNs, nextCheckNs, attempt int64
		var includedInt sql.NullInt64
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
		// Only store fields needed for inclusion decision
		matches = append(matches, trackedMatch{
			txHash:           txHash,
			firstSeenNs:      firstSeenNs,
			included:         included,
			blockNumberPtr:   blockNumberPtr,
			transactionIndex: transactionIndexPtr,
		})
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("error iterating tracked transactions: %w", err)
	}

	log.Debug("Found tracked transactions in block", "count", len(matches))

	// Check each transaction in the block against our tracked transactions.
	// Point 4: inclusion facts (block number, hash, timestamp) are stored
	// UNCONDITIONALLY for every tracked transaction found in a block.
	// Point 4: "first_seen < block time" is a SEPARATE derived flag (recorded as
	// inclusion_flag), not part of the inclusion storage decision.
	// Point 6: the resolver never writes to tracked_transactions or any observation
	// table — it only writes to transaction_inclusion.
	blockTimeNs := int64(block.Time()) * int64(time.Second)
	for i, txHash := range txHashes {
		// Linear search - fast for small match sets (~130 typical)
		for _, m := range matches {
			if bytesEqual(txHash, m.txHash) {
				// Derived flag: was the transaction first seen before this block's time?
				firstSeenBeforeBlock := m.firstSeenNs < blockTimeNs

				// Store inclusion fact unconditionally (points 4 & 6).
				if err := r.storeInclusionResult(
					m.txHash,
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

				log.Debug("Stored inclusion fact", "tx_hash", hex.EncodeToString(txHash), "block_number", blockNumber,
					"first_seen_before_block", firstSeenBeforeBlock)
				break
			}
		}
		// If transaction not in our tracked set, we ignore it (not our concern).
	}

	// Explicitly clear slices to help GC (ponytail: manual clear for memory pressure)
	txHashes = nil
	matches = nil

	return nil
}

// bytesEqual compares two byte slices for equality.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	// If using finalized tag, fetch the finalized head once to validate
	var finalizedHead *types.Header
	if r.useFinalizedTag {
		var rawHeader types.Header
		err := r.rpcClient.Client().CallContext(ctx, &rawHeader, "eth_getBlockByNumber", "finalized", false)
		if err == nil {
			finalizedHead = &rawHeader
		}
	}

	for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
		// Check context
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue
		}

		// Always fetch by explicit block number
		block, err := r.rpcClient.BlockByNumber(ctx, big.NewInt(blockNum))
		if err != nil {
			continue
		}
		if block == nil {
			continue
		}

		// Validate block number matches request
		if block.NumberU64() != uint64(blockNum) {
			continue
		}

		// If we have a finalized head, ensure the block is finalized (or we're OK with non-finalized for recent blocks)
		if finalizedHead != nil && block.NumberU64() > finalizedHead.Number.Uint64() {
			// Don't skip - just continue. The resolver processes all blocks in range.
		}

		// Process block
		if err := r.processBlock(ctx, block); err != nil {
			continue
		}
	}

	return nil
}
