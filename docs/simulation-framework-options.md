# Simulation Framework Options for Peer Dropping Strategies

This document outlines candidate approaches to address the core limitation of trace-replay simulators: they cannot predict what new peer connections would form after dropping an existing peer in the simulation. The goal is to evaluate random vs. usefulness-based peer dropping strategies while being transparent about assumptions.

## Problem Statement
In a trace-replay simulator using recorded mempool/transaction data:
- We know which peers connected and when in the original trace.
- If we simulate dropping a peer at time T, we cannot know whether the node would have formed a new connection to replace it in reality.
- This leads to potentially pessimistic estimates if we assume no replacement occurs.

## Candidate Approaches

### 1. No-Replacement / Pessimistic Baseline
**Approach:** When a peer is dropped in the simulation, it is simply removed and not replaced. Peer count decreases monotonically over time due to drops.

**Assumptions:**
- Dropped peers are never replaced by new connections.
- The only peer changes are those explicitly dropped by our strategy plus natural disconnects from the trace.
- New connections that would have occurred organically in real operation are ignored.

**What it can't capture:**
- The natural peer churn and replacement that occurs in real Ethereum nodes.
- Any beneficial effects where dropping unresponsive peers allows faster replacement by better peers.
- The dynamic equilibrium of peer count that nodes typically maintain.

**How hard to build:** Trivial. Simply omit any logic for generating new peer connections beyond those in the original trace. Can be implemented by modifying the peer event replay to skip dropped peers and not inject new connect events.

**Reusability for network-wide analysis:** High. This approach establishes a clear lower bound for performance. For network-wide studies, we could apply the same baseline consistently across all simulated nodes.

### 2. Empirical Resampling from Historical Trace
**Approach:** When a peer is dropped at time T, sample a replacement peer connection from the set of peers that actually connected to our node in similar time windows elsewhere in the recorded trace.

**Assumptions:**
- Peer connection patterns are stationary enough that historical connection events can predict future ones.
- The time of day, recent network conditions, or other contextual factors from the trace can be used to find similar windows.
- Dropped peers are replaced by peers with similar characteristics (geography, client type, etc.) as would occur naturally.

**What it can't capture:**
- Changes in network topology over time that make historical patterns unreliable.
- The specific peer selection logic of our node (we're assuming it mirrors historical averages).
- Cases where dropped peers leave a gap that isn't filled by similar peers (e.g., dropping all peers from a specific geographic region).

**How hard to build:** Moderate. Requires:
1. Indexing peer connect events in the trace by time and features (if available).
2. For each drop event, querying similar time windows to get candidate replacement peers.
3. Sampling from those candidates (possibly weighted by recency or similarity).
4. Injecting a synthetic peer connect event into the simulation at the drop time.
5. Managing the lifecycle of the simulated replacement peer (should it disconnect naturally?).

**Reusability for network-wide analysis:** Medium. The resampling logic depends on having a trace of connections *to the simulated node*. For network-wide analysis with multiple nodes, we would need either:
- Separate traces for each node (impractical), or
- A synthetic trace generation model based on aggregate statistics.
The core sampling technique could be reused if we develop such a model.

### 3. Statistical Arrival-Rate Model
**Approach:** Model peer connection attempts as a stochastic process (e.g., Poisson process) fit from historical inter-connection times in the trace. When a peer is dropped, schedule future connection attempts according to this model.

**Assumptions:**
- Peer connection attempts follow a random process with computable rate parameters.
- The historical trace provides sufficient data to estimate connection rate (and possibly more complex distributions like time-of-day variation).
- Each connection attempt results in a peer with characteristics sampled from the historical distribution of observed peers.

**What it can't capture:**
- Correlations in connection attempts (e.g., bursts after network partitions).
- Strategic peer selection (we're assuming connections are random samples from historical peers).
- Changes in peer availability due to external events not reflected in our trace window.

**How hard to build:** Moderate to High. Requires:
1. Analyzing the trace to compute connection attempt rate (possibly as a function of time).
2. Fitting a distribution (exponential for Poisson, or more complex) to inter-connection times.
3. Implementing a stochastic event scheduler that generates synthetic connect events based on the model.
4. Sampling peer attributes (if modeled) from historical data.
5. Ensuring the model doesn't generate unrealistic connection bursts or droughts.

**Reusability for network-wide analysis:** High. A connection attempt model based on aggregate network behavior (rather than node-specific history) could be shared across all simulated nodes. This approach aligns naturally with the supervisor's suggestion of modeling based on incoming-connection history.

## Common Design Principles for Reusability

Regardless of which approach is chosen, the following should be designed as reusable modules:

1. **Trace Replay Engine:** Separate the concerns of:
   - Reading recorded transaction/gossip events
   - Applying dropping strategies to peer connections
   - Generating synthetic peer events (for approaches 2 & 3)
   - This allows the same engine to be used for single-node and later multi-node simulation.

2. **Peer Usefulness Scoring:** Extract the logic for computing transaction usefulness (e.g., based on inclusion speed, gas price, etc.) into a package that can be used by:
   - The dropping strategy to decide which peers to drop
   - The analyzer to evaluate post-drop useful transaction receipt
   - Network-wide analysis to assess emergent usefulness patterns

3. **Drop Decision Interface:** Define a clear interface for dropping strategies:
   ```go
   type DroppingStrategy interface {
       // ShouldDrop returns true if the peer should be dropped at this time
       ShouldDrop(peer PeerInfo, simulationTime time.Time, txObs []TxObservation) bool
       // Optional: called when a peer is actually dropped (for cleanup)
       OnDrop(peer PeerInfo, simulationTime time.Time)
   }
   ```
   This makes it easy to swap random vs. usefulness-based approaches and experiment with others.

4. **Metrics Collection:** Standardize what the simulation records:
   - Useful transactions received over time
   - Peer count dynamics
   - Drop/replace events
   - Connection attempt statistics
   This ensures comparability between approaches and readiness for network-wide extension.

## Recommendation for Brainstorming
For the initial simulation focused on evaluating dropping strategies, I recommend starting with Approach #1 (No-Replacement) to establish a clear baseline, then implementing Approach #3 (Statistical Arrival-Rate) as it best matches the supervisor's suggestion and offers the best path to network-wide extension. Approach #2 (Empirical Resampling) is useful as a middle ground but introduces complex dependency on the specific trace being replayed.

None of these approaches should block future network-wide analysis if we maintain clean separation between:
- Core simulation mechanics (event timing, transaction processing)
- Peer connection modeling (drop strategies + connection generation)
- Usefulness scoring and analysis