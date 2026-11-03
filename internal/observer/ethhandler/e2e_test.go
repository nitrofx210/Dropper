package ethhandler

import (
	"io"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"

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

// TestPeerConnAgeMs verifies peer_conn_age_ms recorded at ingest: the first
// announcement right after handshake is ~0ms, a later announcement (after a
// short delay) is larger, and a peer with no recorded handshake stores NULL.
func TestPeerConnAgeMs(t *testing.T) {
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

	// Record peerA's handshake completion now; peerB never handshakes.
	peerAID := peerA.ID().String()
	h.peerHandshakeMu.Lock()
	h.peerHandshakeNs[peerAID] = time.Now().UnixNano()
	h.peerHandshakeMu.Unlock()

	hash := crypto.Keccak256Hash([]byte("conn-age-tx"))
	announcement := &ethprot.NewPooledTransactionHashesPacket72{
		Hashes: []common.Hash{hash},
		Types:  []uint8{0},
		Sizes:  []uint32{0},
		Mask:   types.CustodyBitmap{},
	}

	type sendResult struct{ err error }

	// First announcement right after handshake: conn age ~0ms.
	ch1 := make(chan sendResult, 1)
	go func() { ch1 <- sendResult{err: p2p.Send(rwA, ethprot.NewPooledTransactionHashesMsg, announcement)} }()
	msg1, err := rwB.ReadMsg()
	if err != nil {
		t.Fatalf("read first announcement: %v", err)
	}
	if err := h.handleMessage(peerA, msg1, rwA, ethprot.ETH72); err != nil {
		t.Fatalf("handleMessage first: %v", err)
	}
	if res := <-ch1; res.err != nil {
		t.Fatalf("peer A first send: %v", res.err)
	}

	// Delay so the second announcement lands at a clearly larger conn age.
	time.Sleep(150 * time.Millisecond)

	// Second announcement after a delay: conn age must be larger than the first.
	ch2 := make(chan sendResult, 1)
	go func() { ch2 <- sendResult{err: p2p.Send(rwA, ethprot.NewPooledTransactionHashesMsg, announcement)} }()
	msg2, err := rwB.ReadMsg()
	if err != nil {
		t.Fatalf("read second announcement: %v", err)
	}
	if err := h.handleMessage(peerA, msg2, rwA, ethprot.ETH72); err != nil {
		t.Fatalf("handleMessage second: %v", err)
	}
	if res := <-ch2; res.err != nil {
		t.Fatalf("peer A second send: %v", res.err)
	}

	// peerB never handshaked: conn age must be NULL.
	ch3 := make(chan sendResult, 1)
	go func() { ch3 <- sendResult{err: p2p.Send(rwB, ethprot.NewPooledTransactionHashesMsg, announcement)} }()
	msg3, err := rwA.ReadMsg()
	if err != nil {
		t.Fatalf("read peerB announcement: %v", err)
	}
	if err := h.handleMessage(peerB, msg3, rwB, ethprot.ETH72); err != nil {
		t.Fatalf("handleMessage peerB: %v", err)
	}
	if res := <-ch3; res.err != nil {
		t.Fatalf("peer B send: %v", res.err)
	}

	// Assert conn ages in insertion order.
	rows, err := sto.DB().Query(`SELECT peer_conn_age_ms FROM transaction_observations ORDER BY observation_id`)
	if err != nil {
		t.Fatalf("query conn ages: %v", err)
	}
	defer rows.Close()

	var ages []*int64
	for rows.Next() {
		var age *int64
		if err := rows.Scan(&age); err != nil {
			t.Fatalf("scan conn age: %v", err)
		}
		ages = append(ages, age)
	}
	if len(ages) != 3 {
		t.Fatalf("expected 3 observation rows, got %d", len(ages))
	}

	if ages[0] == nil {
		t.Fatalf("first announcement conn age must not be NULL (~0ms expected)")
	}
	if *ages[0] > 50 {
		t.Errorf("first announcement conn age should be ~0ms, got %dms", *ages[0])
	}
	if ages[1] == nil {
		t.Fatalf("second announcement conn age must not be NULL (> first expected)")
	}
	if *ages[1] <= *ages[0] {
		t.Errorf("second announcement conn age (%dms) must be larger than first (%dms)", *ages[1], *ages[0])
	}
	if ages[1] != nil && *ages[1] < 100 {
		t.Errorf("second announcement conn age should be ~150ms after delay, got %dms", *ages[1])
	}
	if ages[2] != nil {
		t.Errorf("peerB (no handshake) conn age must be NULL, got %dms", *ages[2])
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

// TestE2EAnnouncementsBroadcastsPooledResponse tests the full flow:
// 1. Peer A sends NewPooledTransactionHashes announcement
// 2. Peer B sends Transactions (full broadcast)
// 3. Peer C sends PooledTransactions (response to GetPooledTransactions)
func TestE2EAnnouncementsBroadcastsPooledResponse(t *testing.T) {
	sto, err := storage.New("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()

	// Use fetchMode=true to handle PooledTransactions
	h := New(sto, 1, "test-observer", true, false, 0)
	h.SetRunContext(1, "test-observer")

	// Set up chain info for handshake
	genesis := types.NewBlock(&types.Header{
		Number: common.Big0,
	}, &types.Body{}, nil, nil)
	headHash := crypto.Keccak256Hash([]byte("head"))
	h.SetChainInfo(1, genesis, 100, headHash, 1234567890)

	// Create three peer pipes
	rwA, rwB := p2p.MsgPipe()
	rwC, rwD := p2p.MsgPipe()
	rwE, rwF := p2p.MsgPipe()
	defer func() {
		_ = rwA.Close(); _ = rwB.Close()
		_ = rwC.Close(); _ = rwD.Close()
		_ = rwE.Close(); _ = rwF.Close()
	}()

	peerA := p2p.NewPeerPipe(testPeerID(1), "peerA", nil, rwA)
	peerB := p2p.NewPeerPipe(testPeerID(2), "peerB", nil, rwC)
	peerC := p2p.NewPeerPipe(testPeerID(3), "peerC", nil, rwE)

	// Test 1: NewPooledTransactionHashes announcement from peer A
	txHash1 := crypto.Keccak256Hash([]byte("announcement-tx"))
	announcement := &ethprot.NewPooledTransactionHashesPacket71{
		Hashes: []common.Hash{txHash1},
		Types:  []uint8{0},
		Sizes:  []uint32{0},
	}

	type sendResult struct{ err error }
	ch := make(chan sendResult, 1)
	go func() { ch <- sendResult{err: p2p.Send(rwA, ethprot.NewPooledTransactionHashesMsg, announcement)} }()
	msg, err := rwB.ReadMsg()
	if err != nil {
		t.Fatalf("read announcement: %v", err)
	}
	if err := h.handleMessage(peerA, msg, rwA, ethprot.ETH71); err != nil {
		t.Fatalf("handleMessage announcement: %v", err)
	}
	if res := <-ch; res.err != nil {
		t.Fatalf("peer A send: %v", res.err)
	}

	// Test 2: Transactions (full broadcast) from peer B
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    0,
		GasPrice: common.Big1,
		Gas:      21000,
		To:       func() *common.Address { a := common.HexToAddress("0x1234"); return &a }(),
		Value:    common.Big1,
		Data:     nil,
	})
	txHash2 := tx.Hash()
	txList := []*types.Transaction{tx}
	txListRLP, _ := rlp.EncodeToRawList(txList)
	var transactionsPkt ethprot.TransactionsPacket
	// TransactionsPacket embeds RawList, so we copy the encoded data
	transactionsPkt.AppendList(&txListRLP)

	ch = make(chan sendResult, 1)
	go func() { ch <- sendResult{err: p2p.Send(rwC, ethprot.TransactionsMsg, &transactionsPkt)} }()
	msg, err = rwD.ReadMsg()
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	if err := h.handleMessage(peerB, msg, rwC, ethprot.ETH71); err != nil {
		t.Fatalf("handleMessage transactions: %v", err)
	}
	if res := <-ch; res.err != nil {
		t.Fatalf("peer B send: %v", res.err)
	}

	// Test 3: PooledTransactions response from peer C
	tx3 := types.NewTx(&types.LegacyTx{
		Nonce:    1,
		GasPrice: common.Big1,
		Gas:      21000,
		To:       func() *common.Address { a := common.HexToAddress("0x5678"); return &a }(),
		Value:    common.Big1,
		Data:     nil,
	})
	txHash3 := tx3.Hash()
	tx3List := []*types.Transaction{tx3}
	tx3ListRLP, _ := rlp.EncodeToRawList(tx3List)
	pooledPkt := ethprot.PooledTransactionsPacket{
		RequestId: 1,
		List:      tx3ListRLP,
	}

	ch = make(chan sendResult, 1)
	go func() { ch <- sendResult{err: p2p.Send(rwE, ethprot.PooledTransactionsMsg, &pooledPkt)} }()
	msg, err = rwF.ReadMsg()
	if err != nil {
		t.Fatalf("read pooled transactions: %v", err)
	}
	if err := h.handleMessage(peerC, msg, rwE, ethprot.ETH71); err != nil {
		t.Fatalf("handleMessage pooled transactions: %v", err)
	}
	if res := <-ch; res.err != nil {
		t.Fatalf("peer C send: %v", res.err)
	}

	// Verify all three observations were stored
	var totalCount int
	err = sto.DB().QueryRow(`SELECT COUNT(*) FROM transaction_observations`).Scan(&totalCount)
	if err != nil {
		t.Fatalf("query total observations: %v", err)
	}
	if totalCount != 3 {
		t.Errorf("expected 3 total observations, got %d", totalCount)
	}

	// Verify each tx_hash has exactly one row
	for _, h := range []common.Hash{txHash1, txHash2, txHash3} {
		var c int
		err = sto.DB().QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE tx_hash = ?`, h.Bytes()).Scan(&c)
		if err != nil {
			t.Fatalf("query observation for %x: %v", h, err)
		}
		if c != 1 {
			t.Errorf("expected 1 row for tx %x, got %d", h, c)
		}
	}

	// Verify message types are correct
	var msgTypes []string
	rows, err := sto.DB().Query(`SELECT message_type FROM transaction_observations ORDER BY observation_id`)
	if err != nil {
		t.Fatalf("query message_types: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mt string
		if err := rows.Scan(&mt); err != nil {
			t.Fatalf("scan message_type: %v", err)
		}
		msgTypes = append(msgTypes, mt)
	}
	// Should have: announcement, direct_broadcast, pooled_tx_response
	expected := []string{"announcement", "direct_broadcast", "pooled_tx_response"}
	if len(msgTypes) != 3 {
		t.Fatalf("expected 3 message types, got %d", len(msgTypes))
	}
	for i, mt := range msgTypes {
		if mt != expected[i] {
			t.Errorf("observation %d: expected message_type %s, got %s", i, expected[i], mt)
		}
	}
}