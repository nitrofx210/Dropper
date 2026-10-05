package main

// eclipse_model.go — routing-table / bucket-filling model of an Ethereum-style
// eclipse attack, used to compute the attacker's capture ratio under different
// peer-admission policies.
//
// This is a *structural* model of go-ethereum's Kademlia table (17 buckets,
// 16 live slots + 10 replacements each). It does NOT use simnet (simnet only
// simulates links). It is the statistical layer; simnet is the link layer.
//
// IMPORTANT CAVEAT: this model uses the strict "fill every live slot" criterion,
// which is a CONSERVATIVE upper bound on attacker cost. Real attacks are more
// efficient — inserting even one node per bucket can dominate discovery lookups
// (the "False Friends" attack eclipses with just two /24s even under the subnet
// cap). So the resource counts below are worst-case; actual eclipse is cheaper.
// That gap is itself a useful threat-model finding.

import (
	crand "crypto/rand"
	"fmt"
	"math/rand"
)

const (
	numBuckets  = 17
	bucketSize  = 16
	replaceSize = 10
)

var rng = rand.New(rand.NewSource(42)) // deterministic for reproducible runs

func ri(n int) int {
	if n <= 0 {
		return 0
	}
	return rng.Intn(n)
}

func randID() [32]byte {
	var b [32]byte
	crand.Read(b[:])
	return b
}

type enode struct {
	id      [32]byte
	ip24    string
	attacker bool
}

// bucketOf maps a peer ID to its Kademlia bucket index (0 = farthest,
// numBuckets-1 = closest). Ethereum keeps buckets for distances 2^239..2^255.
func bucketOf(self, peer [32]byte) int {
	var x [32]byte
	for i := 0; i < 32; i++ {
		x[i] = self[i] ^ peer[i]
	}
	highest := -1
	for i := 0; i < 32; i++ {
		b := x[i]
		if b == 0 {
			continue
		}
		for j := 7; j >= 0; j-- {
			if b&(1<<uint(j)) != 0 {
				highest = (31-i)*8 + j // x[0] is the most-significant byte -> bit 255
				break
			}
		}
		if highest >= 0 {
			break
		}
	}
	if highest < 0 {
		return numBuckets - 1 // identical IDs -> closest bucket
	}
	if highest < 239 {
		return 0
	}
	return highest - 239
}

// byteIdx / bitMask convert a bit position (255 = MSB of x[0]) to its byte and
// in-byte mask, consistent with bucketOf's numbering.
func byteIdx(p int) int    { return 31 - p/8 }
func bitMask(p int) byte   { return 1 << uint(p%8) }

// idForBucket generates a node ID whose bucket (relative to self) is exactly b.
// This mirrors the real eclipse trick: the attacker generates key pairs offline
// until the ID lands in the bucket it wants. Here we construct the ID directly.
func idForBucket(self [32]byte, b int) [32]byte {
	k := b + 239
	if k > 255 {
		k = 255
	}
	if k < 0 {
		k = 0
	}
	id := self
	// bits above k: keep equal to self so (self XOR id) is 0 there.
	// bit k: flip so (self XOR id) has its most-significant 1 at position k.
	id[byteIdx(k)] ^= bitMask(k)
	// bits below k: randomize (does not affect the MSB position).
	for p := k - 1; p >= 0; p-- {
		if ri(2) == 1 {
			id[byteIdx(p)] |= bitMask(p)
		} else {
			id[byteIdx(p)] &^= bitMask(p)
		}
	}
	return id
}

// emptiestBucket returns the bucket with the fewest live slots (greedy targeting).
func (t *routingTable) emptiestBucket() int {
	best, bestLive := 0, len(t.buckets[0].live)
	for b := 1; b < numBuckets; b++ {
		if len(t.buckets[b].live) < bestLive {
			bestLive = len(t.buckets[b].live)
			best = b
		}
	}
	return best
}

type bucket struct {
	live         []enode
	replacements []enode
}

type routingTable struct {
	self            [32]byte
	buckets         [numBuckets]bucket
	bucketSubnetCnt map[int]map[string]int
	tableSubnetCnt  map[string]int
	bucketIPLimit   int // 0 = no limit
	tableIPLimit    int // 0 = no limit
}

func newTable(self [32]byte, bucketIPLimit, tableIPLimit int) *routingTable {
	return &routingTable{
		self:            self,
		bucketSubnetCnt: make(map[int]map[string]int),
		tableSubnetCnt:  make(map[string]int),
		bucketIPLimit:   bucketIPLimit,
		tableIPLimit:    tableIPLimit,
	}
}

func (t *routingTable) canAdd(b int, ip24 string) bool {
	if t.bucketIPLimit > 0 && t.bucketSubnetCnt[b][ip24] >= t.bucketIPLimit {
		return false
	}
	if t.tableIPLimit > 0 && t.tableSubnetCnt[ip24] >= t.tableIPLimit {
		return false
	}
	return true
}

// add inserts a node into the live set (or replacement list) if caps allow.
func (t *routingTable) add(n enode) bool {
	b := bucketOf(t.self, n.id)
	if !t.canAdd(b, n.ip24) {
		return false
	}
	if len(t.buckets[b].live) < bucketSize {
		t.buckets[b].live = append(t.buckets[b].live, n)
	} else if len(t.buckets[b].replacements) < replaceSize {
		t.buckets[b].replacements = append(t.buckets[b].replacements, n)
	} else {
		return false
	}
	if t.bucketSubnetCnt[b] == nil {
		t.bucketSubnetCnt[b] = make(map[string]int)
	}
	t.bucketSubnetCnt[b][n.ip24]++
	t.tableSubnetCnt[n.ip24]++
	return true
}

// reserveHonest pins k honest nodes into the closest bucket so the victim always
// retains some honest peers (the P5 static-quorum mitigation).
func (t *routingTable) reserveHonest(k int) {
	for i := 0; i < k; i++ {
		t.buckets[numBuckets-1].live = append(
			t.buckets[numBuckets-1].live,
			enode{id: randID(), ip24: fmt.Sprintf("10.0.%d.0", i), attacker: false},
		)
	}
}

// fillAttacker fills an (initially empty) table with attacker nodes, respecting
// the subnet caps. The attacker targets the emptiest bucket each iteration
// (greedy, as a real attacker would) and generates a matching ID. If uniqueIP is
// true, every node gets its own /24 (models the P3 IP<->key binding measure).
func (t *routingTable) fillAttacker(subnets []string, uniqueIP bool) int {
	added := 0
	ipc := 0
	for attempt := 0; attempt < 500000; attempt++ {
		id := idForBucket(t.self, t.emptiestBucket())
		var ip24 string
		if uniqueIP {
			ipc++
			ip24 = fmt.Sprintf("30.%d.0.0", ipc) // unique per node
		} else {
			if len(subnets) == 0 {
				break
			}
			ip24 = subnets[ri(len(subnets))]
		}
		if t.add(enode{id: id, ip24: ip24, attacker: true}) {
			added++
		}
	}
	return added
}

// capture returns attacker-controlled live slots and the total live slots.
func (t *routingTable) capture() (attackerLive, total int) {
	total = numBuckets * bucketSize
	for b := 0; b < numBuckets; b++ {
		for _, n := range t.buckets[b].live {
			if n.attacker {
				attackerLive++
			}
		}
	}
	return
}

// fillEfficient models the "False Friends" attack: instead of filling every
// live slot, the attacker inserts a MAJORITY of nodes in each bucket so that
// discovery lookups (which pick the closest known nodes) return mostly attacker
// peers. Filling >bucketSize/2 slots per bucket suffices to dominate selection.
// Returns the number of buckets the attacker dominates and how many nodes it used.
func (t *routingTable) fillEfficient(subnets []string, uniqueIP bool) (dominated, nodesUsed int) {
	majority := bucketSize/2 + 1 // 9 of 16
	ipc := 0
	for b := 0; b < numBuckets; b++ {
		// account for honest nodes already pinned in this bucket (P5 quorum)
		honest := 0
		for _, n := range t.buckets[b].live {
			if !n.attacker {
				honest++
			}
		}
		target := majority
		if honest >= majority {
			target = honest + 1 // must out-number the honest quorum
		}
		placed := 0
		for attempt := 0; attempt < 100000 && placed < target; attempt++ {
			id := idForBucket(t.self, b)
			var ip24 string
			if uniqueIP {
				ipc++
				ip24 = fmt.Sprintf("30.%d.0.0", ipc)
			} else {
				if len(subnets) == 0 {
					return
				}
				ip24 = subnets[ri(len(subnets))]
			}
			if t.add(enode{id: id, ip24: ip24, attacker: true}) {
				placed++
				nodesUsed++
			}
		}
		// bucket is dominated if attacker live nodes are a strict majority
		att := 0
		for _, n := range t.buckets[b].live {
			if n.attacker {
				att++
			}
		}
		if att*2 > len(t.buckets[b].live) {
			dominated++
		}
	}
	return
}

func mkSubnets(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("30.%d.0.0", i))
	}
	return out
}
