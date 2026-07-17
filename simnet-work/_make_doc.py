import zipfile, os

out = r"C:\Users\kingd\simnet-work\PROGRESS_SUMMARY.docx"

paras = [
    ("P2P Network Simulator - Progress Summary", True),
    ("Date: 2026-07-16. Goal: wire two go-ethereum p2p.Server nodes over marcopolo/simnet and run a test. A small personal tool, not a full server.", False),
    ("1. Completed", True),
    ("- go-ethereum (...\\go-ethereum) and marcopolo/simnet (...\\simnet) cloned in the prior session; both build and their tests pass.", False),
    ("- Patched geth p2p.Config to expose a listener hook: added `ListenFunc func(network, addr string) (net.Listener, error)` to p2p.Config (config.go) and made Server.Start() use it (falling back to net.Listen) in server.go. This lets an external package inject a simnet-backed listener without touching geth internals.", False),
    ("2. In-progress: simtcp stream layer (the \"simtep\" piece)", True),
    ("- Implemented geth-simnet/simtcp/simtcp.go: a connection-oriented, multiplexed byte stream (net.Listener + net.Conn) over simnet's datagram (net.PacketConn) transport.", False),
    ("- Frame: 1-byte flags (SYN/ACK/DATA/FIN) + 4-byte connID + 4-byte payload length + payload. Per-node demux goroutine. SYN/SYN-ACK connect handshake; DATA chunked to < MTU; FIN closes. Listener.Addr()/Conn.Addr() return *net.TCPAddr (geth requires this).", False),
    ("- SimNet binds one simnet endpoint per node address; ListenFunc returns a listener for a pre-created node; Dialer implements p2p.NodeDialer, resolving the peer's TCP endpoint to the simnet address.", False),
    ("3. Open task: wire two nodes + run test", True),
    ("- Wrote geth-simnet/two_node_test.go: one simnet.Simnet (20 ms latency), two simtcp nodes (127.0.0.1:30303, :30304), two p2p.Server instances - A listens (simtcp listener), B static-dials A (simtcp dialer). A minimal \"sim\" protocol exchanges ping/pong; the test asserts PeerCount==1 and that \"ping\" crossed the link.", False),
    ("- Updated geth-simnet/go.mod (require + replace go-ethereum => ../go-ethereum) and ran go mod tidy successfully.", False),
    ("4. Key decisions", True),
    ("- Minimal geth patch via Config.ListenFunc rather than a white-box test inside p2p.", False),
    ("- Reuse geth's existing Dialer (NodeDialer) hook for outbound; only the listener needed the patch.", False),
    ("- Node addresses match what geth advertises (fallback 127.0.0.1 + listener port) so the dialer lookup hits the right simnet node.", False),
    ("5. Code changes", True),
    ("- go-ethereum/p2p/config.go: +ListenFunc field, +\"net\" import.", False),
    ("- go-ethereum/p2p/server.go: Start() defaults listenFunc from Config.ListenFunc.", False),
    ("- geth-simnet/go.mod: +require/replace go-ethereum => ../go-ethereum.", False),
    ("- geth-simnet/simtcp/simtcp.go: new stream layer. geth-simnet/two_node_test.go: new test.", False),
    ("6. Current blocker / unresolved", True),
    ("- Test FAILS. The RLPx encryption handshake completes and peers connect (logs show \"Adding p2p peer ... peercount=1\"), but the first protocol message read returns EOF (\"invalid message: (code 0) (size 0) EOF\"), dropping the connection.", False),
    ("- Hypothesis: stream misalignment - rlpx reads a frame with fsize=0 for the ping, implying extra/missing bytes before the protocol message. Not yet root-caused.", False),
    ("- Temporary fmt.Printf debug instrumentation is still present in simtcp.go and two_node_test.go and must be removed once fixed.", False),
]

def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

body = []
for text, bold in paras:
    runs = '<w:r><w:rPr><w:b/></w:rPr><w:t xml:space="preserve">%s</w:t></w:r>' % esc(text) if bold else \
           '<w:r><w:t xml:space="preserve">%s</w:t></w:r>' % esc(text)
    body.append('<w:p>%s</w:p>' % runs)
document = (
    '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
    '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
    '<w:body>' + ''.join(body) + '</w:body></w:document>'
)

content_types = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>' \
    '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">' \
    '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>' \
    '<Default Extension="xml" ContentType="application/xml"/>' \
    '<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>' \
    '</Types>'

rels = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>' \
    '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">' \
    '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>' \
    '</Relationships>'

with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("[Content_Types].xml", content_types)
    z.writestr("_rels/.rels", rels)
    z.writestr("word/document.xml", document)

print("wrote", out, os.path.getsize(out), "bytes")
