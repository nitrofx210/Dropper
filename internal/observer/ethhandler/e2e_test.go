package ethhandler

import (
	"io"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// observeTxHashesOnPipe runs one protocol version over a MsgPipe pair: two peers
// each send a NewPooledTransactionHashes announcement containing txHash. It
// asserts the same hash from two peers produces two rows (duplicates preserved)
// in transaction_observations.
//
// p2p.Send blocks until the payload is consumed, so each send runs in its own
// goroutine while this goroutine reads at the far end.
func observeTxHashesOnPipe(t *testing.T, version uint, txHash common.Hash) {
	t.Helper()

	sto, err := storage.New("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()

	h := New(sto, 1, "test-observer", false, false, 0)
	h.SetRunContext(1, "test-observer")

	rwA, rwB := p2p.MsgPipe()
	defer func() { _ = rwA.Close(); _ = rwB.Close() }()

	peerA := p2p.NewPeerPipe(testPeerID(1), "peerA", nil, rwA)
	peerB := p2p.NewPeerPipe(testPeerID(2), "peerB", nil, rwB)

	// ETH72 announces with an extra custody-mask field; earlier versions use
	// the 3-field packet.
	var announcement any
	if version == ethprot.ETH72 {
		announcement = &ethprot.NewPooledTransactionHashesPacket72{
			Hashes: []common.Hash{txHash},
			Types:  []uint8{0},
			Sizes:  []uint32{0},
			Mask:   types.CustodyBitmap{},
		}
	} else {
		announcement = &ethprot.NewPooledTransactionHashesPacket71{
			Hashes: []common.Hash{txHash},
			Types:  []uint8{0},
			Sizes:  []uint32{0},
		}
	}

	type sendResult struct {
		err error
	}
	chA := make(chan sendResult, 1)
	chB := make(chan sendResult, 1)
	go func() { chA <- sendResult{err: p2p.Send(rwA, ethprot.NewPooledTransactionHashesMsg, announcement)} }()
	go func() { chB <- sendResult{err: p2p.Send(rwB, ethprot.NewPooledTransactionHashesMsg, announcement)} }()

	// Peer A's message arrives at B's read end; dispatch with peerA's identity.
	msgA, err := rwB.ReadMsg()
	if err != nil {
		t.Fatalf("read A's message at B: %v", err)
	}
	if err := h.handleMessage(peerA, msgA, rwA, version); err != nil {
		t.Fatalf("handleMessage A: %v", err)
	}
	if res := <-chA; res.err != nil {
		t.Fatalf("peer A send: %v", res.err)
	}

	// Peer B's message arrives at A's read end; dispatch with peerB's identity.
	msgB, err := rwA.ReadMsg()
	if err != nil {
		t.Fatalf("read B's message at A: %v", err)
	}
	if err := h.handleMessage(peerB, msgB, rwB, version); err != nil {
		t.Fatalf("handleMessage B: %v", err)
	}
	if res := <-chB; res.err != nil {
		t.Fatalf("peer B send: %v", res.err)
	}

	// Assert exactly two rows for the same hash (one per peer).
	rows, err := sto.DB().Query(`SELECT COUNT(DISTINCT peer_id), COUNT(*) FROM transaction_observations WHERE tx_hash = ?`, txHash.Bytes())
	if err != nil {
		t.Fatalf("query observations: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no observation rows returned")
	}
	var distinctPeers, totalRows int
	if err := rows.Scan(&distinctPeers, &totalRows); err != nil {
		t.Fatalf("scan observation rows: %v", err)
	}
	if distinctPeers != 2 {
		t.Errorf("expected 2 distinct peers, got %d", distinctPeers)
	}
	if totalRows != 2 {
		t.Errorf("expected 2 rows (duplicates preserved), got %d", totalRows)
	}
}

func TestNewPooledTxHashesAcrossVersions(t *testing.T) {
	txHash := crypto.Keccak256Hash([]byte("duplicate-tx-from-two-peers"))

	for _, version := range []uint{ethprot.ETH69, ethprot.ETH70, ethprot.ETH71, ethprot.ETH72} {
		t.Run("eth/"+itob(version), func(t *testing.T) {
			observeTxHashesOnPipe(t, version, txHash)
		})
	}
}

// TestDecodeFailureDoesNotKillConnection verifies that an undecodable
// NewPooledTransactionHashes announcement is discarded, counted via decodeFailure,
// and does not kill the connection: a subsequent valid announcement is still stored.
func TestDecodeFailureDoesNotKillConnection(t *testing.T) {
	sto, err := storage.New("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()

	h := New(sto, 1, "test-observer", false, false, 0)
	h.SetRunContext(1, "test-observer")

	rwA, rwB := p2p.MsgPipe()
	defer func() { _ = rwA.Close(); _ = rwB.Close() }()

	peerA := p2p.NewPeerPipe(testPeerID(1), "peerA", nil, rwA)

	// Garbage announcement (not valid RLP for NewPooledTransactionHashes).
	// decodeFailure counts it and logs every 30s, but does not kill the
	// connection: the handle loop logs the error and keeps reading.
	gr := garbageReader([]byte{0xff, 0xff, 0xff})
	badMsg := p2p.Msg{Code: ethprot.NewPooledTransactionHashesMsg, Payload: &gr, Size: 3}
	if err := h.handleMessage(peerA, badMsg, rwA, ethprot.ETH72); err == nil {
		t.Fatalf("decode failure should return an error (counted), got nil")
	}

	// Subsequent valid announcement must still be stored.
	txHash := common.HexToHash("0xdeadbeef")
	type sendResult struct {
		err error
	}
	ch := make(chan sendResult, 1)
	go func() { ch <- sendResult{err: p2p.Send(rwA, ethprot.NewPooledTransactionHashesMsg, &ethprot.NewPooledTransactionHashesPacket72{
		Hashes: []common.Hash{txHash},
		Types:  []uint8{0},
		Sizes:  []uint32{0},
	})} }()

	msg, err := rwB.ReadMsg()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if err := h.handleMessage(peerA, msg, rwA, ethprot.ETH72); err != nil {
		t.Fatalf("handleMessage valid: %v", err)
	}
	if res := <-ch; res.err != nil {
		t.Fatalf("peer A send: %v", res.err)
	}

	rows, err := sto.DB().Query(`SELECT COUNT(*) FROM transaction_observations WHERE tx_hash = ?`, txHash.Bytes())
	if err != nil {
		t.Fatalf("query observations: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no observation rows returned")
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after decode failure + valid announcement, got %d", count)
	}
}

// testPeerID returns a distinct enode.ID (32 bytes) for each peer.
func testPeerID(n byte) enode.ID {
	var id enode.ID
	id[0] = n
	return id
}

// garbageReader wraps a byte slice as an io.Reader for Msg.Payload. It returns
// io.EOF once all bytes have been consumed so Discard can terminate.
type garbageReader []byte

func (g *garbageReader) Read(p []byte) (int, error) {
	if len(*g) == 0 {
		return 0, io.EOF
	}
	n := copy(p, *g)
	*g = (*g)[n:]
	return n, nil
}

// itob formats a uint as a decimal string.
func itob(v uint) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}