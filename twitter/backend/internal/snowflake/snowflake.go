// Package snowflake generates 64-bit, roughly time-ordered, unique IDs.
//
// Layout (à la Twitter Snowflake):
//
//	 63          22          12           0
//	+--+----------------------+----------+------------+
//	|0 |  41-bit ms since epoch| 10-bit   | 12-bit seq |
//	|  |                       | machine  |            |
//	+--+----------------------+----------+------------+
//
// Because the timestamp occupies the high bits, IDs sort chronologically. This
// lets the home-timeline merge be a plain numeric sort and makes keyset
// pagination ("give me ids < cursor") trivial.
package snowflake

import (
	"sync"
	"time"
)

const (
	machineBits = 10
	seqBits     = 12

	maxMachine = -1 ^ (-1 << machineBits) // 1023
	maxSeq     = -1 ^ (-1 << seqBits)     // 4095

	machineShift = seqBits
	timeShift    = seqBits + machineBits
)

// DefaultEpoch is the custom epoch (2024-01-01T00:00:00Z) in milliseconds. A
// recent epoch maximizes the usable lifetime of the 41-bit timestamp.
const DefaultEpoch int64 = 1704067200000

// Generator hands out unique IDs for a single machine. It is safe for
// concurrent use.
type Generator struct {
	epoch    int64
	machine  int64
	mu       sync.Mutex
	lastMS   int64
	seq      int64
	nowMilli func() int64 // injectable clock, for tests
}

// New returns a Generator for the given machine id (0..1023). Ids out of range
// are masked into range rather than rejected, since a bad machine id is a
// deployment mistake, not a runtime condition.
func New(machine int64) *Generator {
	return &Generator{
		epoch:    DefaultEpoch,
		machine:  machine & maxMachine,
		nowMilli: func() int64 { return time.Now().UnixMilli() },
	}
}

// Next returns the next unique ID. When more than maxSeq ids are requested
// within the same millisecond it spins until the clock advances, so ids stay
// strictly increasing within a process.
func (g *Generator) Next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.nowMilli()
	if now == g.lastMS {
		g.seq = (g.seq + 1) & maxSeq
		if g.seq == 0 {
			// Sequence exhausted this ms; wait for the next tick.
			for now <= g.lastMS {
				now = g.nowMilli()
			}
		}
	} else {
		g.seq = 0
	}
	g.lastMS = now

	return ((now - g.epoch) << timeShift) | (g.machine << machineShift) | g.seq
}
