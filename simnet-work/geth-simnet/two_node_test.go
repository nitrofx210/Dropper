package gethsimnet_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	simnet "github.com/marcopolo/simnet"

	"github.com/kingd/geth-simnet/simtcp"
)

// received carries the payload the listener node got from the dialer so the
// test can assert the message actually traversed the simulated link.
var received = make(chan string, 1)

// quit keeps the protocol goroutines alive for the duration of the test. geth
// tears down a peer as soon as its protocol handler returns, so to assert that
// the peers are connected *and* exchange a message, the handlers block on quit
// until the test is done. Closed by TestTwoNodesOverSimnet's deferred cleanup.
var quit = make(chan struct{})

// simProto is a tiny devp2p protocol: the dialer sends "ping", the listener
// (inbound) replies "pong".
//
// Messages are sent with p2p.Send, which computes and sets Msg.Size from the
// RLP-encoded payload. Constructing a p2p.Msg literal by hand without setting
// Size is a trap: the transport copies exactly Size bytes of the Payload, so a
// zero Size silently transmits an empty message.
var simProto = p2p.Protocol{
	Name:    "sim",
	Version: 1,
	Length:  1,
	Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
		if p.Inbound() {
			msg, err := rw.ReadMsg()
			if err != nil {
				return err
			}
			var s string
			if err := msg.Decode(&s); err != nil {
				return err
			}
			received <- s
			if err := p2p.Send(rw, 0, "pong"); err != nil {
				return err
			}
			<-quit // keep the peer up until the test finishes
			return nil
		}
		if err := p2p.Send(rw, 0, "ping"); err != nil {
			return err
		}
		msg, err := rw.ReadMsg()
		if err != nil {
			return err
		}
		var s string
		if err := msg.Decode(&s); err != nil {
			return err
		}
		<-quit // keep the peer up until the test finishes
		return nil
	},
}

func TestTwoNodesOverSimnet(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, slog.LevelDebug, false)))

	// 1) Build the simulated network with a little latency.
	sim := &simnet.Simnet{LatencyFunc: simnet.StaticLatency(20 * time.Millisecond)}
	sim.Start()
	defer sim.Close()

	net := simtcp.NewSimNet(sim)

	// 2) Create two simtcp endpoints. Addresses must match what geth will
	//    advertise (it uses the listener address's IP/port).
	aNode, err := net.Node("127.0.0.1:30303")
	if err != nil {
		t.Fatalf("create node A: %v", err)
	}
	bNode, err := net.Node("127.0.0.1:30304")
	if err != nil {
		t.Fatalf("create node B: %v", err)
	}

	keyA, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	// 3) Node A: listens (inbound) over the simtcp listener.
	srvA := &p2p.Server{
		Config: p2p.Config{
			PrivateKey:      keyA,
			Name:            "A",
			ListenAddr:      "127.0.0.1:30303",
			ListenFunc:      net.ListenFunc,
			Dialer:          simtcp.NewDialer(aNode),
			MaxPeers:        10,
			MaxPendingPeers: 10,
			NoDiscovery:     true,
			NodeDatabase:    filepath.Join(t.TempDir(), "A"),
			Protocols:       []p2p.Protocol{simProto},
		},
	}
	if err := srvA.Start(); err != nil {
		t.Fatalf("start A: %v", err)
	}
	defer srvA.Stop()

	// 4) Learn A's enode so B can dial it as a static node.
	infoA := srvA.NodeInfo()
	t.Logf("A enode=%s ip=%s listenerPort=%d", infoA.Enode, infoA.IP, infoA.Ports.Listener)
	nodeA, err := enode.ParseV4(infoA.Enode)
	if err != nil {
		t.Fatalf("parse A enode: %v", err)
	}

	// 5) Node B: dialer only, static-dials A over the simtcp dialer.
	srvB := &p2p.Server{
		Config: p2p.Config{
			PrivateKey:      keyB,
			Name:            "B",
			ListenAddr:      "", // B does not listen
			Dialer:          simtcp.NewDialer(bNode),
			MaxPeers:        10,
			MaxPendingPeers: 10,
			NoDiscovery:     true,
			StaticNodes:     []*enode.Node{nodeA},
			NodeDatabase:    filepath.Join(t.TempDir(), "B"),
			Protocols:       []p2p.Protocol{simProto},
		},
	}
	if err := srvB.Start(); err != nil {
		t.Fatalf("start B: %v", err)
	}
	defer srvB.Stop()

	// 6) Wait for the RLPx handshake to complete on both sides.
	deadline := time.Now().Add(20 * time.Second)
	for srvA.PeerCount() < 1 || srvB.PeerCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("peers never connected: A=%d B=%d", srvA.PeerCount(), srvB.PeerCount())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("connected: A has %d peer(s), B has %d peer(s)", srvA.PeerCount(), srvB.PeerCount())

	// 7) Wait for the protocol message to traverse the link, then confirm both
	//    peers are still connected (the handlers block on quit, so they stay up).
	select {
	case s := <-received:
		if s != "ping" {
			t.Fatalf("unexpected payload %q", s)
		}
		t.Logf("listener received %q over simnet", s)
	case <-time.After(20 * time.Second):
		t.Fatal("did not receive ping over the simulated network")
	}

	if srvA.PeerCount() < 1 || srvB.PeerCount() < 1 {
		t.Fatalf("peers dropped before assertion: A=%d B=%d", srvA.PeerCount(), srvB.PeerCount())
	}
	t.Logf("connected: A has %d peer(s), B has %d peer(s)", srvA.PeerCount(), srvB.PeerCount())

	// Unblock the protocol handlers so the servers can stop cleanly. This must
	// happen before srvA.Stop()/srvB.Stop() (which wait for the handlers).
	close(quit)
}
