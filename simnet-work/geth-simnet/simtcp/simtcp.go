// Package simtcp provides a connection-oriented, stream-style transport
// (net.Listener + net.Conn) layered on top of a marcopolo/simnet simulated
// network, which only offers a datagram (net.PacketConn) interface.
//
// It exists so that go-ethereum's p2p.Server — which expects a reliable,
// ordered byte stream — can be run entirely inside a simulated network with
// configurable latency/bandwidth, without touching real TCP.
//
// Design notes:
//   - Each simtcp "node" owns one simnet endpoint (a UDP-style address such as
//     127.0.0.1:30303) and a single demultiplexing goroutine.
//   - Connections are identified by a 32-bit connection id carried in every
//     frame. A dialer chooses the id; the listener echoes it back.
//   - The handshake is a minimal SYN / SYN-ACK exchange. Data frames carry a
//     length-prefixed payload; large writes are chunked to fit the link MTU.
//   - simnet delivers packets reliably and in order under normal conditions,
//     so no sequence-number / retransmission machinery is implemented. The
//     framing is enough to give geth a clean stream abstraction.
package simtcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	simnet "github.com/marcopolo/simnet"
)

const (
	flagSYN = 1 << iota
	flagACK
	flagDATA
	flagFIN
)

const (
	// maxPayload is the largest payload carried in a single frame. It is kept
	// safely under simnet's default MTU (1500) minus the 9-byte header.
	maxPayload = 1400
	headerLen  = 9 // flag(1) + connID(4) + payloadLen(4)

	dialTimeout   = 15 * time.Second
	acceptTimeout = 15 * time.Second
)

// SimNet ties a set of simtcp nodes to a single underlying simnet.Simnet so
// they can route packets to one another.
type SimNet struct {
	sim *simnet.Simnet

	mu    sync.Mutex
	nodes map[string]*node // keyed by "ip:port"
}

// NewSimNet wraps an already-Started simnet.Simnet.
//
// The Simnet must be Started before any nodes are created, because node
// creation registers endpoints with the router.
func NewSimNet(sim *simnet.Simnet) *SimNet {
	return &SimNet{
		sim:   sim,
		nodes: make(map[string]*node),
	}
}

// Node creates a simtcp node bound to addr (e.g. "127.0.0.1:30303"). The addr
// must match whatever the simulated application will advertise (geth uses the
// listener address), because dialers look peers up by this exact string.
func (n *SimNet) Node(addr string) (*node, error) {
	udp, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("simtcp: bad node address %q: %w", addr, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.nodes[addr]; ok {
		return nil, fmt.Errorf("simtcp: node %s already exists", addr)
	}
	simConn := n.sim.NewEndpoint(udp, simnet.NodeBiDiLinkSettings{})
	nd := &node{
		net:      n,
		addr:     addr,
		simAddr:  udp,
		sim:      simConn,
		conns:    make(map[uint32]*simtcpConn),
		acceptCh: make(chan *simtcpConn, 8),
		closed:   make(chan struct{}),
	}
	n.nodes[addr] = nd
	go nd.run()
	return nd, nil
}

// ListenFunc is a drop-in replacement for net.Listen that returns a simtcp
// listener for a previously created node. Pass it to p2p.Config.ListenFunc.
func (n *SimNet) ListenFunc(network, addr string) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	nd, ok := n.nodes[addr]
	if !ok {
		return nil, fmt.Errorf("simtcp: no node for listen address %s; create it with SimNet.Node first", addr)
	}
	if nd.listener != nil {
		return nil, fmt.Errorf("simtcp: %s is already listening", addr)
	}
	nd.listener = &tcpListener{node: nd, closed: make(chan struct{})}
	return nd.listener, nil
}

// Dialer dials outbound peer connections from a specific local simtcp node.
// It implements p2p.NodeDialer.
type Dialer struct {
	local *node
}

// NewDialer returns a Dialer that sends from the given local node.
func NewDialer(local *node) *Dialer {
	return &Dialer{local: local}
}

// Dial implements p2p.NodeDialer. It resolves the peer's TCP endpoint to a
// simtcp address and opens a stream connection to it.
func (d *Dialer) Dial(ctx context.Context, dest *enode.Node) (net.Conn, error) {
	ap, ok := dest.TCPEndpoint()
	if !ok {
		return nil, errors.New("simtcp: peer has no TCP endpoint")
	}
	remoteKey := ap.String() // e.g. "127.0.0.1:30303"

	d.local.net.mu.Lock()
	remote, ok := d.local.net.nodes[remoteKey]
	d.local.net.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("simtcp: no node for dial target %s", remoteKey)
	}

	connID := atomic.AddUint32(&d.local.nextConnID, 1)
	c := newConn(d.local, remote.simAddr, connID)
	d.local.mu.Lock()
	d.local.conns[connID] = c
	d.local.mu.Unlock()

	if err := d.local.sendFrame(remote.simAddr, flagSYN, connID, nil); err != nil {
		d.local.unregister(connID)
		return nil, err
	}

	select {
	case <-c.connected:
		return c, nil
	case <-ctx.Done():
		d.local.unregister(connID)
		return nil, ctx.Err()
	case <-time.After(dialTimeout):
		d.local.unregister(connID)
		return nil, fmt.Errorf("simtcp: dial %s timed out", remoteKey)
	}
}

// node is one endpoint in the simulated network.
type node struct {
	net     *SimNet
	addr    string
	simAddr *net.UDPAddr
	sim     *simnet.SimConn

	mu         sync.Mutex
	conns      map[uint32]*simtcpConn
	listener   *tcpListener
	nextConnID uint32

	acceptCh chan *simtcpConn
	closed   chan struct{}
}

func (nd *node) run() {
	buf := make([]byte, 1500)
	for {
		n, from, err := nd.sim.ReadFrom(buf)
		if err != nil {
			// The simnet endpoint was closed (sim.Close or node close).
			nd.mu.Lock()
			if nd.listener != nil {
				nd.listener.Close()
			}
			nd.mu.Unlock()
			return
		}
		flags, connID, payload, derr := decodeFrame(buf[:n])
		if derr != nil {
			continue
		}
		nd.handleFrame(flags, connID, payload, from)
	}
}

func (nd *node) handleFrame(flags byte, connID uint32, payload []byte, from net.Addr) {
	nd.mu.Lock()
	c := nd.conns[connID]
	listener := nd.listener
	nd.mu.Unlock()

	switch {
	case flags&flagSYN != 0 && flags&flagACK == 0:
		// Inbound connection request.
		if listener == nil || c != nil {
			return
		}
		nd.mu.Lock()
		if nd.conns[connID] == nil {
			c = newConn(nd, from, connID)
			nd.conns[connID] = c
		} else {
			c = nd.conns[connID]
		}
		nd.mu.Unlock()

		// Acknowledge so the dialer can proceed.
		_ = nd.sendFrame(from, flagSYN|flagACK, connID, nil)
		select {
		case nd.acceptCh <- c:
		case <-nd.closed:
		}

	case flags&flagACK != 0:
		// SYN-ACK for a dialer-side connection.
		if c != nil {
			c.markConnected()
		}

	case flags&flagDATA != 0:
		if c != nil {
			c.deliver(payload)
		}

	case flags&flagFIN != 0:
		if c != nil {
			c.remoteClosed()
		}
	}
}

func (nd *node) sendFrame(to net.Addr, flags byte, connID uint32, payload []byte) error {
	frame := encodeFrame(flags, connID, payload)
	_, err := nd.sim.WriteTo(frame, to)
	return err
}

func (nd *node) unregister(id uint32) {
	nd.mu.Lock()
	delete(nd.conns, id)
	nd.mu.Unlock()
}

// tcpListener is a net.Listener backed by a simtcp node.
type tcpListener struct {
	node      *node
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *tcpListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.node.acceptCh:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *tcpListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *tcpListener) Addr() net.Addr {
	return tcpAddr(l.node.simAddr)
}

// simtcpConn is a connection-oriented stream between two simtcp nodes.
type simtcpConn struct {
	node       *node
	remoteAddr net.Addr // peer's simnet (UDP) address
	connID     uint32

	readCh   chan []byte
	readBuf  []byte
	readMu   sync.Mutex
	readDone chan struct{} // closed when the remote half is closed

	connected   chan struct{}
	connectOnce sync.Once

	closed    chan struct{}
	closeOnce sync.Once

	writeMu sync.Mutex
}

func newConn(nd *node, remote net.Addr, id uint32) *simtcpConn {
	return &simtcpConn{
		node:       nd,
		remoteAddr: remote,
		connID:     id,
		readCh:     make(chan []byte, 64),
		readDone:   make(chan struct{}),
		connected:  make(chan struct{}),
		closed:     make(chan struct{}),
	}
}

func (c *simtcpConn) markConnected() {
	c.connectOnce.Do(func() { close(c.connected) })
}

func (c *simtcpConn) deliver(payload []byte) {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case c.readCh <- cp:
	case <-c.readDone:
	case <-c.closed:
	}
}

func (c *simtcpConn) remoteClosed() {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if !c.isReadClosed() {
		close(c.readDone)
	}
}

func (c *simtcpConn) isReadClosed() bool {
	select {
	case <-c.readDone:
		return true
	default:
		return false
	}
}

func (c *simtcpConn) Read(b []byte) (int, error) {
	for {
		if len(c.readBuf) > 0 {
			n := copy(b, c.readBuf)
			c.readBuf = c.readBuf[n:]
			return n, nil
		}
		select {
		case data, ok := <-c.readCh:
			if !ok {
				return 0, io.EOF
			}
			c.readBuf = data
		case <-c.readDone:
			return 0, io.EOF
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
}

func (c *simtcpConn) Write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	written := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > maxPayload {
			chunk = chunk[:maxPayload]
		}
		if err := c.node.sendFrame(c.remoteAddr, flagDATA, c.connID, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		b = b[len(chunk):]
	}
	return written, nil
}

func (c *simtcpConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.node.sendFrame(c.remoteAddr, flagFIN, c.connID, nil)
		c.remoteClosed()
		c.node.unregister(c.connID)
	})
	return nil
}

func (c *simtcpConn) LocalAddr() net.Addr {
	return tcpAddr(c.node.simAddr)
}

func (c *simtcpConn) RemoteAddr() net.Addr {
	if u, ok := c.remoteAddr.(*net.UDPAddr); ok {
		return tcpAddr(u)
	}
	return c.remoteAddr
}

func (c *simtcpConn) SetDeadline(time.Time) error      { return nil }
func (c *simtcpConn) SetReadDeadline(time.Time) error  { return nil }
func (c *simtcpConn) SetWriteDeadline(time.Time) error { return nil }

// tcpAddr converts a UDP-style address into a *net.TCPAddr so that geth (which
// type-asserts listener.Addr() to *net.TCPAddr and extracts the IP via
// netutil.AddrAddr) is happy.
func tcpAddr(a *net.UDPAddr) *net.TCPAddr {
	return &net.TCPAddr{IP: a.IP, Port: a.Port, Zone: a.Zone}
}

func encodeFrame(flags byte, connID uint32, payload []byte) []byte {
	frame := make([]byte, headerLen+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], connID)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[headerLen:], payload)
	return frame
}

func decodeFrame(data []byte) (flags byte, connID uint32, payload []byte, err error) {
	if len(data) < headerLen {
		return 0, 0, nil, errors.New("simtcp: frame shorter than header")
	}
	flags = data[0]
	connID = binary.BigEndian.Uint32(data[1:5])
	pl := binary.BigEndian.Uint32(data[5:9])
	if uint32(len(data)-headerLen) < pl {
		return 0, 0, nil, errors.New("simtcp: frame payload truncated")
	}
	payload = data[headerLen : headerLen+int(pl)]
	return flags, connID, payload, nil
}
