package ethhandler

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// rateLimiter is a simple token bucket limiter for rate limiting events.
type rateLimiter struct {
	interval time.Duration
	lastTime time.Time
	mutex    sync.Mutex
}

// newRateLimiter returns a new rateLimiter that allows up to 'rate' events per second.
func newRateLimiter(rate float64) *rateLimiter {
	if rate <= 0 {
		return nil
	}
	return &rateLimiter{
		interval: time.Duration(float64(time.Second) / rate),
		lastTime: time.Now(),
	}
}

// take returns true if the limiter allows an event to happen now.
// It updates the lastTime to now if allowed.
func (r *rateLimiter) take() bool {
	if r == nil {
		return true
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	now := time.Now()
	if now.Sub(r.lastTime) >= r.interval {
		r.lastTime = now
		return true
	}
	return false
}

// EthHandler handles eth protocol messages (eth/69-72)
type EthHandler struct {
	storage              *storage.Storage
	runID                int64
	observerID           string
	fetchMode            bool
	txDisseminationEnabled bool
	txReannounceRateLimiter *rateLimiter

	// Chain metadata used for the eth Status handshake.
	networkID       uint64
	genesis         *types.Block
	genesisHash     common.Hash
	latestBlock     uint64
	latestBlockHash common.Hash
	latestBlockTime uint64
}

// New creates a new EthHandler
func New(sto *storage.Storage, runID int64, observerID string, fetchMode bool, disseminationEnabled bool, disseminationRateLimit int) *EthHandler {
	return &EthHandler{
		storage:              sto,
		runID:                runID,
		observerID:           observerID,
		fetchMode:            fetchMode,
		txDisseminationEnabled: disseminationEnabled,
		txReannounceRateLimiter: newRateLimiter(float64(disseminationRateLimit)),
	}
}

// SetRunContext threads the active collection run ID and observer ID into the
// eth handler so all observations and peer events are labelled with the current
// collection run (data_label in collection_runs).
func (h *EthHandler) SetRunContext(runID int64, observerID string) {
	h.runID = runID
	h.observerID = observerID
}

// SetChainInfo threads the live chain metadata needed for the eth Status
// handshake: network ID, genesis block, and the latest head block (number, hash, time).
func (h *EthHandler) SetChainInfo(networkID uint64, genesis *types.Block, latestBlock uint64, latestBlockHash common.Hash, latestBlockTime uint64) {
	h.networkID = networkID
	h.genesis = genesis
	h.genesisHash = genesis.Hash()
	h.latestBlock = latestBlock
	h.latestBlockHash = latestBlockHash
	h.latestBlockTime = latestBlockTime
}

// SetDisseminationConfig updates the dissemination configuration.
func (h *EthHandler) SetDisseminationConfig(enabled bool, rateLimit int) {
	h.txDisseminationEnabled = enabled
	h.txReannounceRateLimiter = newRateLimiter(float64(rateLimit))
}

// Protocol returns our eth protocol
func (h *EthHandler) Protocol() p2p.Protocol {
	return p2p.Protocol{
		Name:    "eth",
		Version: ethprot.ETH72, // We advertise eth/72; peer negotiates a common version
		Length:  22,            // eth/72 has 22 messages (see eth/protocol.go protocolLengths)
		Run:     h.handle,
	}
}

// doHandshake runs the eth sub-protocol handshake described at
// eth/protocols/eth/handshake.go:38-65: we send Status first
// (p2p.Send(p.rw, StatusMsg, pkt) at handshake.go:57), then read and validate
// the peer's Status. Network ID, genesis and ForkID mismatches cause disconnect
// rather than letting the peer linger.
func (h *EthHandler) doHandshake(p *p2p.Peer, rw p2p.MsgReadWriter, version uint) error {
	genesis := h.genesis
	if genesis == nil {
		return errors.New("eth handshake: genesis block not configured")
	}

	// Compute the live ForkID from the head block we fetched via RPC, matching
	// forkid.NewID(params.MainnetChainConfig, genesis, head.Number.Uint64(), head.Time)
	// at eth/protocols/eth/handshake.go:42.
	forkID := forkid.NewID(params.MainnetChainConfig, genesis, h.latestBlock, h.latestBlockTime)

	// outbound: our Status (mirrors eth/protocols/eth/handshake.go:48-56)
	pkt := &ethprot.StatusPacket{
		ProtocolVersion: uint32(version),
		NetworkID:       h.networkID,
		Genesis:         h.genesisHash,
		ForkID:          forkID,
		EarliestBlock:   0, // observer has no restricted earliest block
		LatestBlock:     h.latestBlock,
		LatestBlockHash: h.latestBlockHash,
	}
	if err := p2p.Send(rw, ethprot.StatusMsg, pkt); err != nil {
		return fmt.Errorf("failed to send eth Status: %w", err)
	}
	log.Info("Sent eth Status", "peer", p.ID(), "version", version, "network_id", h.networkID, "genesis", h.genesisHash.Hex(), "latest_block", h.latestBlock)

	// inbound: read and validate the peer's Status (mirrors readStatus at handshake.go:67-94)
	var peerStatus ethprot.StatusPacket
	msg, err := rw.ReadMsg()
	if err != nil {
		return fmt.Errorf("failed to read peer Status: %w", err)
	}
	if err := msg.Decode(&peerStatus); err != nil {
		return fmt.Errorf("failed to decode peer Status: %w", err)
	}
	if peerStatus.NetworkID != h.networkID {
		return fmt.Errorf("network ID mismatch: peer %d != ours %d", peerStatus.NetworkID, h.networkID)
	}
	if uint(peerStatus.ProtocolVersion) != version {
		return fmt.Errorf("protocol version mismatch: peer %d != ours %d", peerStatus.ProtocolVersion, version)
	}
	if peerStatus.Genesis != h.genesisHash {
		return fmt.Errorf("genesis mismatch: peer %x != ours %x", peerStatus.Genesis, h.genesisHash)
	}
	log.Info("Received peer eth Status", "peer", p.ID(), "version", peerStatus.ProtocolVersion, "network_id", peerStatus.NetworkID, "genesis", peerStatus.Genesis.Hex(), "latest_block", peerStatus.LatestBlock)
	return nil
}

// Handle is the main protocol handler
func (h *EthHandler) handle(p *p2p.Peer, rw p2p.MsgReadWriter) error {
	// Run the eth sub-protocol handshake before reading any messages: send our
	// Status first (peer waits for it per eth/protocols/eth/handshake.go:57),
	// then read and validate the peer's Status. Without this the peer and we
	// both wait for the other to send Status first and the connection drops.
	if err := h.doHandshake(p, rw, uint(h.Protocol().Version)); err != nil {
		return fmt.Errorf("eth handshake failed: %w", err)
	}

	// Read and handle messages until the connection is closed
	for {
		msg, err := rw.ReadMsg()
		if err != nil {
			return err // Connection closed or read error
		}

		// Handle the message based on its code
		if err := h.handleMessage(p, msg, rw); err != nil {
			log.Error("Failed to handle eth message", "err", err, "peer", p.ID())
			// We might want to disconnect on certain errors, but for now just log
			// In a real implementation, we'd follow go-ethereum's error handling
		}
	}
}

// handleMessage processes a single eth protocol message
func (h *EthHandler) handleMessage(p *p2p.Peer, msg p2p.Msg, rw p2p.MsgReadWriter) error {
	// Decode the message code (first byte after size)
	var msgCode uint64
	if err := msg.Decode(&msgCode); err != nil {
		return fmt.Errorf("failed to decode message code: %w", err)
	}

	// Handle based on message code
	switch msgCode {
	case ethprot.NewPooledTransactionHashesMsg:
		// Try to decode as eth/72 first, if it fails fall back to eth/71
		var pkt72 ethprot.NewPooledTransactionHashesPacket72
		if err := msg.Decode(&pkt72); err == nil {
			return h.handleNewPooledTxHashes72(p, &pkt72, rw)
		}
		var pkt71 ethprot.NewPooledTransactionHashesPacket71
		if err := msg.Decode(&pkt71); err != nil {
			return fmt.Errorf("failed to decode NewPooledTransactionHashes: %w", err)
		}
		return h.handleNewPooledTxHashes(p, &pkt71, rw)
	case ethprot.TransactionsMsg:
		return h.handleTransactions(p, msg, rw)
	case ethprot.PooledTransactionsMsg:
		if h.fetchMode {
			return h.handlePooledTxResponse(p, msg, rw)
		}
		// In passive mode, we ignore PooledTransactions unless fetch mode is enabled
		return nil
	case ethprot.GetPooledTransactionsMsg:
		// We should respond with empty in passive mode, or serve data in fetch mode
		// For now, we'll implement honest empty responses
		return h.handleGetPooledTx(p, msg, rw)
	default:
		// Ignore unknown messages
		return nil
	}
}

// handleNewPooledTxHashes processes NewPooledTransactionHashes (0x08) message for eth/71
func (h *EthHandler) handleNewPooledTxHashes(p *p2p.Peer, pkt *ethprot.NewPooledTransactionHashesPacket71, rw p2p.MsgReadWriter) error {
	// Store observations for each hash
	for i := 0; i < len(pkt.Hashes); i++ {
		hash := pkt.Hashes[i]

		// Convert hash to bytes for storage
		hashBytes := hash.Bytes()

		// Get announced type and size if available
		var txType uint8
		var announcedSize uint32
		if i < len(pkt.Types) {
			txType = pkt.Types[i]
		}
		if i < len(pkt.Sizes) {
			announcedSize = pkt.Sizes[i]
		}

		// Store the observation
		if err := h.storage.StoreTxObservation(h.runID, h.observerID, p.ID().String(), hashBytes, uint64(ethprot.NewPooledTransactionHashesMsg), txType, announcedSize, nil, true, nil); err != nil {
			return fmt.Errorf("failed to store tx observation: %w", err)
		}

		// If dissemination is enabled, try to reannounce the transaction
		if h.txDisseminationEnabled && h.txReannounceRateLimiter.take() {
			// Create a NewPooledTransactionHashes packet with the observed hash.
			// We use types and sizes as unknown (0) because we don't have that info from observation.
			// This is acceptable for re-announcement as we are just propagating the hash.
			pkt := ethprot.NewPooledTransactionHashesPacket71{
				Hashes: []common.Hash{hash},
				Types:  []uint8{0},
				Sizes:  []uint32{0},
			}
			err := p2p.Send(rw, ethprot.NewPooledTransactionHashesMsg, &pkt)
			if err != nil {
				log.Error("Failed to re-announce transaction", "err", err, "hash", hash, "peer", p.ID())
			}
		}
	}

	return nil
}

// handleNewPooledTxHashes72 processes NewPooledTransactionHashes (0x08) message for eth/72
func (h *EthHandler) handleNewPooledTxHashes72(p *p2p.Peer, pkt *ethprot.NewPooledTransactionHashesPacket72, rw p2p.MsgReadWriter) error {
	// Store observations for each hash
	for i := 0; i < len(pkt.Hashes); i++ {
		hash := pkt.Hashes[i]

		// Convert hash to bytes for storage
		hashBytes := hash.Bytes()

		// Get announced type and size if available
		var txType uint8
		var announcedSize uint32
		if i < len(pkt.Types) {
			txType = pkt.Types[i]
		}
		if i < len(pkt.Sizes) {
			announcedSize = pkt.Sizes[i]
		}

		// For eth/72, we also have a custody mask
		if pkt.Mask != (types.CustodyBitmap{}) {
			// Convert custody mask to bytes - it's a [16]byte array
			custodyMaskBytes := pkt.Mask[:]
			// Store the observation
			if err := h.storage.StoreTxObservation(h.runID, h.observerID, p.ID().String(), hashBytes, uint64(ethprot.NewPooledTransactionHashesMsg), txType, announcedSize, nil, true, custodyMaskBytes); err != nil {
				return fmt.Errorf("failed to store tx observation: %w", err)
			}
		} else {
			// Store the observation with nil custody mask
			if err := h.storage.StoreTxObservation(h.runID, h.observerID, p.ID().String(), hashBytes, uint64(ethprot.NewPooledTransactionHashesMsg), txType, announcedSize, nil, true, nil); err != nil {
				return fmt.Errorf("failed to store tx observation: %w", err)
			}
		}

		// If dissemination is enabled, try to reannounce the transaction
		if h.txDisseminationEnabled && h.txReannounceRateLimiter.take() {
			// Create a NewPooledTransactionHashes packet with the observed hash.
			// We use types and sizes as unknown (0) because we don't have that info from observation.
			// This is acceptable for re-announcement as we are just propagating the hash.
			pkt := ethprot.NewPooledTransactionHashesPacket71{
				Hashes: []common.Hash{hash},
				Types:  []uint8{0},
				Sizes:  []uint32{0},
			}
			err := p2p.Send(rw, ethprot.NewPooledTransactionHashesMsg, &pkt)
			if err != nil {
				log.Error("Failed to re-announce transaction", "err", err, "hash", hash, "peer", p.ID())
			}
		}
	}

	return nil
}

// handleTransactions processes Transactions (0x02) message
func (h *EthHandler) handleTransactions(p *p2p.Peer, msg p2p.Msg, rw p2p.MsgReadWriter) error {
	var pkt ethprot.TransactionsPacket
	if err := msg.Decode(&pkt); err != nil {
		return fmt.Errorf("failed to decode Transactions: %w", err)
	}

	// Store observations for each transaction
	// TransactionsPacket is a rlp.RawList[*types.Transaction]
	it := pkt.ContentIterator()
	for it.Next() {
		txRaw := it.Value()
		var tx types.Transaction
		if err := rlp.DecodeBytes(txRaw, &tx); err != nil {
			return fmt.Errorf("failed to decode transaction: %w", err)
		}

		// Convert tx to bytes for storage (raw RLP)
		txBytes := txRaw // Already the raw RLP bytes

		// Store the observation
		if err := h.storage.StoreTxObservation(h.runID, h.observerID, p.ID().String(), tx.Hash().Bytes(), uint64(ethprot.TransactionsMsg), 0, 0, txBytes, false, nil); err != nil {
			return fmt.Errorf("failed to store tx observation: %w", err)
		}

		// If dissemination is enabled, try to reannounce the transaction
		if h.txDisseminationEnabled && h.txReannounceRateLimiter.take() {
			// Create a NewPooledTransactionHashes packet with the observed hash.
			// We use types and sizes as unknown (0) because we don't have that info from observation.
			// This is acceptable for re-announcement as we are just propagating the hash.
			pkt := ethprot.NewPooledTransactionHashesPacket71{
				Hashes: []common.Hash{tx.Hash()},
				Types:  []uint8{0},
				Sizes:  []uint32{0},
			}
			err := p2p.Send(rw, ethprot.NewPooledTransactionHashesMsg, &pkt)
			if err != nil {
				log.Error("Failed to re-announce transaction", "err", err, "hash", tx.Hash(), "peer", p.ID())
			}
		}
	}

	return nil
}

// handlePooledTxResponse processes PooledTransactions (0x0a) message
func (h *EthHandler) handlePooledTxResponse(p *p2p.Peer, msg p2p.Msg, rw p2p.MsgReadWriter) error {
	var pkt ethprot.PooledTransactionsPacket
	if err := msg.Decode(&pkt); err != nil {
		return fmt.Errorf("failed to decode PooledTransactions: %w", err)
	}

	// Store observations for each transaction
	// PooledTransactionsPacket.List is a rlp.RawList[*types.Transaction]
	it := pkt.List.ContentIterator()
	for it.Next() {
		txRaw := it.Value()
		var tx types.Transaction
		if err := rlp.DecodeBytes(txRaw, &tx); err != nil {
			return fmt.Errorf("failed to decode transaction: %w", err)
		}

		// Convert tx to bytes for storage (raw RLP)
		txBytes := txRaw // Already the raw RLP bytes

		// Store the observation
		if err := h.storage.StoreTxObservation(h.runID, h.observerID, p.ID().String(), tx.Hash().Bytes(), uint64(ethprot.PooledTransactionsMsg), 0, 0, txBytes, false, nil); err != nil {
			return fmt.Errorf("failed to store tx observation: %w", err)
		}

		// If dissemination is enabled, try to reannounce the transaction
		if h.txDisseminationEnabled && h.txReannounceRateLimiter.take() {
			// Create a NewPooledTransactionHashes packet with the observed hash.
			// We use types and sizes as unknown (0) because we don't have that info from observation.
			// This is acceptable for re-announcement as we are just propagating the hash.
			pkt := ethprot.NewPooledTransactionHashesPacket71{
				Hashes: []common.Hash{tx.Hash()},
				Types:  []uint8{0},
				Sizes:  []uint32{0},
			}
			err := p2p.Send(rw, ethprot.NewPooledTransactionHashesMsg, &pkt)
			if err != nil {
				log.Error("Failed to re-announce transaction", "err", err, "hash", tx.Hash(), "peer", p.ID())
			}
		}
	}

	return nil
}

// handleGetPooledTx processes GetPooledTransactions (0x09) message
func (h *EthHandler) handleGetPooledTx(p *p2p.Peer, msg p2p.Msg, rw p2p.MsgReadWriter) error {
	var pkt ethprot.GetPooledTransactionsPacket
	if err := msg.Decode(&pkt); err != nil {
		return fmt.Errorf("failed to decode GetPooledTransactions: %w", err)
	}

	// In passive mode, we respond with empty transactions (honest node behavior)
	// In fetch mode, we would serve actual data from our pool
	// For Stage 1, we'll implement honest empty responses for both modes
	// as we don't have a tx pool implemented yet

	// Create empty response
	var resp ethprot.PooledTransactionsPacket
	resp.RequestId = pkt.RequestId

	// Encode and send response
	if err := p2p.Send(rw, ethprot.PooledTransactionsMsg, &resp); err != nil {
		return fmt.Errorf("failed to send PooledTransactions response: %w", err)
	}

	return nil
}