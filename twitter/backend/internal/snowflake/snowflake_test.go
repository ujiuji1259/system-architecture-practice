package snowflake

import (
	"sync"
	"testing"
)

func TestNextIsUniqueAndMonotonic(t *testing.T) {
	g := New(1)
	const n = 100000
	seen := make(map[int64]struct{}, n)
	var prev int64
	for i := 0; i < n; i++ {
		id := g.Next()
		if id <= prev {
			t.Fatalf("id not strictly increasing: %d after %d", id, prev)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = struct{}{}
		prev = id
	}
}

func TestNextIsUniqueUnderConcurrency(t *testing.T) {
	g := New(7)
	const goroutines, per = 16, 5000

	var mu sync.Mutex
	seen := make(map[int64]struct{}, goroutines*per)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, per)
			for j := range local {
				local[j] = g.Next()
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate id %d", id)
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*per {
		t.Fatalf("got %d unique ids, want %d", len(seen), goroutines*per)
	}
}

func TestMachineIDEncodedAndTimeOrdered(t *testing.T) {
	// Two generators with a controllable clock: a later timestamp must yield a
	// larger id regardless of machine id.
	clock := int64(1000)
	mk := func(machine int64) *Generator {
		g := New(machine)
		g.nowMilli = func() int64 { return clock }
		return g
	}
	a := mk(1)
	b := mk(1023)

	id1 := a.Next()
	clock = 1001
	id2 := b.Next()
	if id2 <= id1 {
		t.Fatalf("later id %d should exceed earlier id %d", id2, id1)
	}

	// The machine bits must round-trip.
	if got := (b.Next() >> machineShift) & maxMachine; got != 1023 {
		t.Fatalf("machine bits = %d, want 1023", got)
	}
}

func TestSequenceRollsOverToNextMillisecond(t *testing.T) {
	clock := int64(500)
	g := New(0)
	ticks := 0
	g.nowMilli = func() int64 {
		// Advance only when the generator spins waiting for the next ms.
		ticks++
		if ticks > maxSeq+2 {
			return clock + 1
		}
		return clock
	}
	// Exhaust the sequence within one ms, forcing a spin to the next ms.
	var last int64
	for i := 0; i < maxSeq+2; i++ {
		last = g.Next()
	}
	if ms := (last >> timeShift) + DefaultEpoch; ms != clock+1 {
		t.Fatalf("timestamp = %d, want rollover to %d", ms, clock+1)
	}
}
