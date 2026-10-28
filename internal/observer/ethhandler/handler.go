package ethhandler

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p/enode"
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

	// onPeerDir reports each peer's actual direction (true=outbound) to the
	// observer so connect/disconnect peer events use p.Inbound() rather than
	// the bootnode set.
	onPeerDir func(enode.ID, bool)
}

// SetPeerDirCallback registers the callback that reports each peer's actual direction.
func (h *EthHandler) SetPeerDirCallback(cb func(enode.ID, bool)) {
	h.onPeerDir = cb
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

// ProtocolVersions returns one p2p.Protocol per supported eth version
// (eth/69-72). Each protocol instance carries its own Version so the p2p
// server can negotiate a common version with the peer, and each instance's
// Run closure captures that negotiated version for the Status handshake.
// The StatusPacket struct is identical across versions 69-72
// (eth/protocols/eth/protocol.go:97-106); only message Length differs.
func (h *EthHandler) ProtocolVersions() []p2p.Protocol {
	versions := []uint{ethprot.ETH69, ethprot.ETH70, ethprot.ETH71, ethprot.ETH72}
	protos := make([]p2p.Protocol, 0, len(versions))
	for _, version := range versions {
		protos = append(protos, p2p.Protocol{
			Name:    ethprot.ProtocolName,
			Version: version,
			Length:  ProtocolLength(version),
			Run: func(ver uint) func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				return func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
					return h.handle(p, rw, ver)
				}
			}(version),
		})
	}
	return protos
}

// ProtocolLength returns the number of implemented messages for the given eth
// protocol version (eth/protocol.go protocolLengths).
func ProtocolLength(version uint) uint64 {
	switch version {
	case ethprot.ETH69:
		return 18
	case ethprot.ETH70:
		return 18
	case ethprot.ETH71:
		return 20
	case ethprot.ETH72:
		return 22
	default:
		return 0
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

// parseClient splits an advertised peer name like "Geth/v1.17.7-stable/..."
// into client name ("Geth") and client version ("v1.17.7-stable").
func parseClient(fullname string) (clientName, clientVersion string) {
	parts := strings.SplitN(fullname, "/", 2)
	if len(parts) == 2 {
		clientName = parts[0]
		if rest := strings.SplitN(parts[1], " ", 2); len(rest) > 0 {
			clientVersion = rest[0]
		}
	}
	return clientName, clientVersion
}

// Handle is the main protocol handler. version is the negotiated eth protocol
// version of THIS protocol instance (NOT hardcoded ETH72), captured from the
// p2p.Protocol.Version the server matched during capability negotiation.
func (h *EthHandler) handle(p *p2p.Peer, rw p2p.MsgReadWriter, version uint) error {
	// Run the eth sub-protocol handshake before reading any messages: send our
	// Status first (peer waits for it per eth/protocols/eth/handshake.go:57),
	// then read and validate the peer's Status. Without this the peer and we
	// both wait for the other to send Status first and the connection drops.
	if err := h.doHandshake(p, rw, version); err != nil {
		log.Error("eth handshake failed", "peer", p.ID(), "version", version, "err", err)
		h.storePeerEvent(p, "disconnect", int(version), "", "", fmt.Sprintf("eth handshake failed: %v", err))
		return fmt.Errorf("eth handshake failed: %w", err)
	}

	// peer address/short id for logging
	id := p.ID()
	shortID := fmt.Sprintf("%x", id[:4])
	remoteAddr := p.RemoteAddr().String()
	if remoteAddr == "" {
		remoteAddr = p.Node().URLv4()
	}
	clientName, clientVersion := parseClient(p.Fullname())

	headLog := fmt.Sprintf("block %d (%x)", h.latestBlock, h.latestBlockHash[:4])
	log.Info("eth handshake ok",
		"peer", shortID,
		"client_name", clientName,
		"client_version", clientVersion,
		"eth_version", version,
		"head", headLog,
		"remote", remoteAddr,
	)
	h.storePeerEvent(p, "handshake_ok", int(version), clientName, clientVersion, fmt.Sprintf("version %d head %s peer %x", version, headLog, id[:4]))

	// Report actual direction so connect/disconnect events use p.Inbound().
	if h.onPeerDir != nil {
		h.onPeerDir(p.ID(), !p.Inbound())
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

// storePeerEvent persists a peer lifecycle event for the active collection run.
// It is guarded by a real runID (> 0); before SetRunContext sets one, events are
// silently dropped so nothing is written with placeholder values.
func (h *EthHandler) storePeerEvent(p *p2p.Peer, eventType string, ethVersion int, clientName, clientVersion, detailJSON string) {
	if h.runID <= 0 {
		return
	}
	peerID := p.ID().String()
	direction := "outbound"
	if p.Inbound() {
		direction = "inbound"
	}
	if err := h.storage.StorePeerEvent(
		h.runID,
		h.observerID,
		peerID,
		eventType,
		direction,
		clientName,
		clientVersion,
		peerID, // enode as peer id
		p.RemoteAddr().String(),
		ethVersion,
		detailJSON,
	); err != nil {
		log.Warn("Failed to store peer event", "peer", p.ID(), "event", eventType, "err", err)
	}
}
