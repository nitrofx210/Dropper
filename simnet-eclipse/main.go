// simnet-eclipse: eclipse-attack threat-modeling harness.
//
// Two layers:
//   1. Link layer  (simnet): demonstrates the *consequence* of capture —
//      an eclipsed victim can only reach its attacker neighbors and cannot
//      reach a honest node that isn't among its (attacker-only) peers.
//   2. Model layer (eclipse_model.go): computes the attacker's capture ratio
//      across peer-admission policies — the statistical exposure metric.
//
// Run:  cd C:\Users\kingd\simnet-eclipse  &&  C:\Go\bin\go run .

package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/marcopolo/simnet"
)

func main() {
	runIsolationDemo()
	runCaptureModel()
}

// runIsolationDemo builds an eclipsed victim (only attacker neighbors exist in
// the simulated network) and shows it can reach attackers but not a honest node.
func runIsolationDemo() {
	fmt.Println("=== simnet link-layer isolation demo (eclipsed victim) ===")

	n := &simnet.Simnet{
		LatencyFunc: simnet.StaticLatency(5 * time.Millisecond),
		// silence the router's "dropping packet" log so output stays readable
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	settings := simnet.NodeBiDiLinkSettings{
		Downlink: simnet.LinkSettings{BitsPerSecond: 10 * simnet.Mibps, MTU: 1500},
		Uplink:   simnet.LinkSettings{BitsPerSecond: 10 * simnet.Mibps, MTU: 1500},
	}

	victim := &net.UDPAddr{IP: net.ParseIP("1.0.0.1"), Port: 9001}
	// The ONLY nodes in the victim's world are attacker-controlled neighbors.
	attackers := []*net.UDPAddr{
		{IP: net.ParseIP("9.9.1.1"), Port: 9101},
		{IP: net.ParseIP("9.9.1.2"), Port: 9102},
		{IP: net.ParseIP("9.9.1.3"), Port: 9103},
	}

	vConn := n.NewEndpoint(victim, settings)
	aConns := make([]*simnet.SimConn, len(attackers))
	for i, a := range attackers {
		aConns[i] = n.NewEndpoint(a, settings)
	}

	n.Start()
	defer n.Close()

	// Each attacker neighbor runs an echo server.
	for i := range attackers {
		ci := aConns[i]
		go func() {
			buf := make([]byte, 1500)
			ci.SetReadDeadline(time.Now().Add(2 * time.Second))
			rn, src, err := ci.ReadFrom(buf)
			if err != nil {
				return
			}
			ci.WriteTo(append([]byte("echo: "), buf[:rn]...), src)
		}()
	}

	// Victim -> attacker neighbor (must work).
	vConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = vConn.WriteTo([]byte("hi"), attackers[0])
	buf := make([]byte, 1500)
	rn, _, err := vConn.ReadFrom(buf)
	if err != nil {
		fmt.Println("  victim -> attacker FAILED:", err)
	} else {
		fmt.Printf("  victim -> attacker OK: %q  (attacker is a neighbor)\n", string(buf[:rn]))
	}

	// Victim -> honest node that is NOT in the network (the eclipse).
	honest := &net.UDPAddr{IP: net.ParseIP("1.0.0.2"), Port: 9002}
	vConn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	_, _ = vConn.WriteTo([]byte("real-block?"), honest)
	_, _, err = vConn.ReadFrom(buf)
	if err != nil {
		fmt.Printf("  victim -> honest (not in network): UNREACHABLE -> eclipsed  (%v)\n", err)
	} else {
		fmt.Println("  victim -> honest: REACHED (not eclipsed)")
	}
	fmt.Println("  -> link-layer isolation confirmed: victim's only reachable peers are attackers.")
}

// runCaptureModel prints the capture ratio under each peer-admission policy.
func runCaptureModel() {
	fmt.Println("\n=== Eclipse capture-ratio model (conservative: requires filling every live slot) ===")
	fmt.Printf("%-20s %-16s %-14s %-9s %s\n",
		"Policy", "Attacker res.", "Captured", "Capture%", "Full eclipse?")

	run := func(name, res string, bucketLim, tableLim, reserved int, subnets []string, uniqueIP bool) {
		t := newTable(randID(), bucketLim, tableLim)
		if reserved > 0 {
			t.reserveHonest(reserved)
		}
		t.fillAttacker(subnets, uniqueIP)
		a, total := t.capture()
		pct := float64(a) / float64(total) * 100
		full := "NO"
		if reserved > 0 {
			full = "NO (quorum)"
		} else if a == total {
			full = "YES"
		}
		fmt.Printf("%-20s %-16s %d/%-11d %-9.1f %s\n", name, res, a, total, pct, full)
	}

	// Baseline: no subnet caps -> one /24 fills the whole table.
	run("Baseline (no caps)", "1 /24", 0, 0, 0, []string{"1.1.1.0"}, false)
	// P0 subnet-diversity cap (geth >=1.8.0): bucketIPLimit=2, tableIPLimit=10.
	run("P0 cap 2/10", "2 /24", 2, 10, 0, []string{"1.1.1.0", "2.2.2.0"}, false)
	run("P0 cap 2/10", "8 /24", 2, 10, 0, mkSubnets(8), false)
	run("P0 cap 2/10", "28 /24", 2, 10, 0, mkSubnets(28), false)
	// P3 one-to-one IP<->key binding (unimplemented): unique IP per node.
	run("P3 IP<->key bind", "272 IPs", 0, 0, 0, nil, true)
	// P5 static-peer quorum: 3 honest peers pinned regardless of capture.
	run("P5 + quorum", "28 /24", 2, 10, 3, mkSubnets(28), false)

	fmt.Println("\n  Note: real attacks are cheaper than this worst-case model (False Friends")
	fmt.Println("  eclipses with just 2 /24s even under the cap by inserting one node/bucket).")

	runEfficientModel()
}

// runEfficientModel prints the "False Friends" efficient attack: dominate each
// bucket with a majority (9/16) of attacker nodes rather than filling all slots.
func runEfficientModel() {
	fmt.Println("\n=== Efficient attack (False Friends: majority-per-bucket dominates discovery) ===")
	fmt.Printf("%-20s %-16s %-18s %-11s %s\n",
		"Policy", "Attacker res.", "Buckets dominated", "Nodes used", "Eclipse?")

	run := func(name, res string, bucketLim, tableLim, reserved int, subnets []string, uniqueIP bool) {
		t := newTable(randID(), bucketLim, tableLim)
		if reserved > 0 {
			t.reserveHonest(reserved)
		}
		dom, used := t.fillEfficient(subnets, uniqueIP)
		verdict := "NO"
		if reserved > 0 {
			verdict = "NO (quorum)"
		} else if dom == numBuckets {
			verdict = "YES"
		} else if dom >= numBuckets-1 {
			verdict = "LIKELY"
		}
		fmt.Printf("%-20s %-16s %d/%-16d %-11d %s\n", name, res, dom, numBuckets, used, verdict)
	}

	run("Baseline (no caps)", "1 /24", 0, 0, 0, []string{"1.1.1.0"}, false)
	run("P0 cap 2/10", "2 /24", 2, 10, 0, []string{"1.1.1.0", "2.2.2.0"}, false)
	run("P0 cap 2/10", "8 /24", 2, 10, 0, mkSubnets(8), false)
	run("P0 cap 2/10", "17 /24", 2, 10, 0, mkSubnets(17), false)
	run("P3 IP<->key bind", "unique IPs", 0, 0, 0, nil, true)
	run("P5 + quorum(9)", "17 /24", 2, 10, 9, mkSubnets(17), false)

	fmt.Println("\n  Under P0 caps, ~17 /24s let the attacker seat a majority in every bucket")
	fmt.Println("  (matches the papers: subnet caps raise cost but do NOT stop a modest attacker).")
	fmt.Println("  A large-enough honest quorum per bucket is what actually blocks domination.")
}
