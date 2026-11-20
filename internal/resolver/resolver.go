package resolver

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// Resolver checks transaction inclusion on-chain with backoff strategy.
type Resolver struct {
	storage     *storage.Storage
	rpcClient   *ethclient.Client
	maxAttempts int
	baseDelay   time.Duration
}

// Errors
var (
	ErrEmptyRPCEndpoint = fmt.Errorf("rpc endpoint is empty")
)

// NewResolver creates a new Resolver instance.
func NewResolver(sto *storage.Storage, rpcEndpoint string, maxAttempts int, baseDelay time.Duration) (*Resolver, error) {
	if rpcEndpoint == "" {
		return nil, ErrEmptyRPCEndpoint
	}
	// Dial the RPC endpoint
	client, err := ethclient.Dial(rpcEndpoint)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		storage:     sto,
		rpcClient:   client,
		maxAttempts: maxAttempts,
		baseDelay:   baseDelay,
	}, nil
}

// Close closes the RPC client.
func (r *Resolver) Close() {
	r.rpcClient.Close()
}

// Run starts the resolver loop. It runs until the context is cancelled.
func (r *Resolver) Run(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second) // Check every 5 seconds for due transactions
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.processDueTransactions(ctx); err != nil {
				// In a real implementation, we'd log the error and continue
				// For now, we'll just continue to not break the loop on transient errors
				// But if it's a context error, we should break.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Continue on other errors
			}
		}
	}
}

// processDueTransactions fetches transactions that are due for a check and processes them.
func (r *Resolver) processDueTransactions(ctx context.Context) error {
	now := time.Now().UnixNano()
	txs, err := r.storage.GetTrackedTransactionsDueForCheck(now)
	if err != nil {
		return fmt.Errorf("failed to get tracked transactions: %w", err)
	}
	for _, tx := range txs {
		// Skip if we have already determined inclusion (included != nil) or if we have stopped checking (next_check_ns == 0)
		if tx.Included != nil || tx.NextCheckNs == 0 {
			continue
		}
		// Check the transaction via RPC
		// We'll use eth_getTransactionByHash
		var result map[string]interface{}
		err := r.rpcClient.Client().CallContext(ctx, &result, "eth_getTransactionByHash", commonHashToHex(tx.TxHash))
		if err != nil {
			// RPC error, we'll backoff
			if err := r.handleCheckResult(ctx, tx, false, nil, nil, nil, nil, err); err != nil {
				return err
			}
			continue
		}
		// If result is null or empty, transaction not found (yet)
		if result == nil || len(result) == 0 {
			if err := r.handleCheckResult(ctx, tx, false, nil, nil, nil, nil, nil); err != nil {
				return err
			}
			continue
		}
		// Transaction found, extract block info
		blockNumberHex, ok := result["blockNumber"].(string)
		if !ok {
			// No block number means pending or not found? Actually, if it's in the mempool, blockNumber is null.
			// We'll treat as not included (still pending)
			if err := r.handleCheckResult(ctx, tx, false, nil, nil, nil, nil, nil); err != nil {
				return err
			}
			continue
		}
		blockNumber, err := hexToUint64(blockNumberHex)
		if err != nil {
			if err := r.handleCheckResult(ctx, tx, false, nil, nil, nil, nil, err); err != nil {
				return err
			}
			continue
		}
		blockHashHex, ok := result["blockHash"].(string)
		if !ok {
			if err := r.handleCheckResult(ctx, tx, false, nil, nil, nil, nil, nil); err != nil {
				return err
			}
			continue
		}
		blockHash := commonHexToHash(blockHashHex)
		if blockHash == nil {
			if err := r.handleCheckResult(ctx, tx, false, nil, nil, nil, nil, nil); err != nil {
				return err
			}
			continue
		}
		txIndexHex, ok := result["transactionIndex"].(string)
		if !ok {
			if err := r.handleCheckResult(ctx, tx, false, nil, blockHash, nil, nil, nil); err != nil {
				return err
			}
			continue
		}
		txIndex, err := hexToUint64(txIndexHex)
		if err != nil {
			if err := r.handleCheckResult(ctx, tx, false, nil, blockHash, nil, nil, err); err != nil {
				return err
			}
			continue
		}
		// We have inclusion! Set included=true, and we can compute useful score if we want.
		// For now, we set useful score to NULL.
		if err := r.handleCheckResult(ctx, tx, true, &blockNumber, blockHash, &txIndex, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// handleCheckResult updates the tracking info for a transaction after a check.
func (r *Resolver) handleCheckResult(ctx context.Context, tx *storage.TrackedTransaction, included bool, blockNumber *uint64, blockHash []byte, txIndex *uint64, usefulScore *float64, err error) error {
	var includedPtr *bool
	if err == nil {
		includedPtr = &included
	} // else leave includedPtr as nil (unknown)

	// Determine new attempt count
	newAttempt := tx.Attempt + 1

	// Determine if we should stop trying (if we have exceeded maxAttempts or we have a definitive answer)
	stop := false
	if err != nil {
		// On error, we don't know, so we don't stop unless we've exceeded maxAttempts
		if newAttempt >= r.maxAttempts {
			stop = true
			// We leave included as unknown (nil)
		}
	} else {
		// No error
		if included {
			// We have a definitive answer: included
			stop = true
		} else {
			// Not included (still pending)
			if newAttempt >= r.maxAttempts {
				// We've tried max times and still not included, we assume it's not going to be included (or we give up)
				stop = true
				// We set included to false
				includedPtr = &included
			}
			// else, we continue trying
		}
	}

	// Compute next check time
	var nextCheck int64
	if stop {
		nextCheck = 0 // means we stop checking
	} else {
		// Exponential backoff with jitter
		// delay = baseDelay * (2^(attempt-1)) + random jitter [0, baseDelay)
		// We use attempt as the number of attempts made so far (including the current one)
		// So for the first attempt (after this check, attempt will be 1) we want baseDelay * 2^0 = baseDelay
		// But note: we have already incremented attempt to newAttempt (which is the count including this check)
		// So the exponent should be (newAttempt - 1)
		base := r.baseDelay
		if newAttempt > 1 {
			// exponential factor: 2^(newAttempt-1)
			// We'll compute by shifting, but we'll use a loop for simplicity since newAttempt is small
			multiplier := 1
			for i := 0; i < newAttempt-1; i++ {
				multiplier *= 2
			}
			base = time.Duration(multiplier) * r.baseDelay
		}
		// Add jitter: random amount between 0 and baseDelay
		jitter := time.Duration(rand.Int63n(int64(r.baseDelay)))
		delay := base + jitter
		nextCheck = time.Now().UnixNano() + int64(delay)
	}

	// Convert blockNumber and txIndex to *int64 for storage
	var blockNumberPtr *int64
	if blockNumber != nil {
		n := int64(*blockNumber)
		blockNumberPtr = &n
	}
	var txIndexPtr *int64
	if txIndex != nil {
		n := int64(*txIndex)
		txIndexPtr = &n
	}

	// Update the storage
	if err := r.storage.UpdateTrackedTransaction(
		tx.TxHash,
		time.Now().UnixNano(), // last_checked_ns
		nextCheck,             // next_check_ns
		newAttempt,            // attempt
		includedPtr,           // included
		blockNumberPtr,        // block_number
		blockHash,             // block_hash
		txIndexPtr,            // transaction_index
		usefulScore,           // useful_score
	); err != nil {
		return fmt.Errorf("failed to update tracked transaction: %w", err)
	}

	return nil
}

// Helper functions
func commonHashToHex(h []byte) string {
	// We expect h to be 32 bytes
	if len(h) != 32 {
		return "0x"
	}
	return "0x" + hex.EncodeToString(h)
}

func hexToUint64(s string) (uint64, error) {
	if len(s) < 2 || s[:2] != "0x" {
		return 0, fmt.Errorf("invalid hex string")
	}
	var n uint64
	_, err := fmt.Sscanf(s[2:], "%x", &n)
	return n, err
}

func commonHexToHash(s string) []byte {
	if len(s) < 2 || s[:2] != "0x" {
		return nil
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return nil
	}
	// Ensure it's 32 bytes
	if len(b) != 32 {
		return nil
	}
	return b
}
