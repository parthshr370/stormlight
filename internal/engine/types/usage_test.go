package types

import "testing"

func TestCalculateCost(t *testing.T) {
	// Prices in $/million tokens.
	mc := ModelCost{Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75}

	t.Run("no cache, per-million pricing", func(t *testing.T) {
		u := &Usage{Input: 1_000_000, Output: 2_000_000}
		CalculateCost(mc, u)
		if u.Cost.Input != 3.0 { // 3.0/1e6 * 1e6
			t.Errorf("Input cost = %v, want 3.0", u.Cost.Input)
		}
		if u.Cost.Output != 30.0 { // 15.0/1e6 * 2e6
			t.Errorf("Output cost = %v, want 30.0", u.Cost.Output)
		}
		if u.Cost.Total != 33.0 {
			t.Errorf("Total = %v, want 33.0", u.Cost.Total)
		}
	})

	t.Run("1h cache write billed at 2x INPUT rate", func(t *testing.T) {
		// 1,000,000 cache writes, 400,000 of them 1h-retention.
		u := &Usage{CacheWrite: 1_000_000, CacheWrite1h: new(400_000), CacheRead: 1_000_000}
		CalculateCost(mc, u)
		wantWrite := (mc.CacheWrite*600_000 + mc.Input*2*400_000) / 1_000_000
		if u.Cost.CacheWrite != wantWrite {
			t.Errorf("CacheWrite = %v, want %v", u.Cost.CacheWrite, wantWrite)
		}
		if u.Cost.CacheRead != mc.CacheRead { // 0.30/1e6 * 1e6
			t.Errorf("CacheRead = %v, want %v", u.Cost.CacheRead, mc.CacheRead)
		}
	})

	t.Run("returns computed cost and mutates usage", func(t *testing.T) {
		u := &Usage{Input: 500_000}
		got := CalculateCost(mc, u)
		if got != u.Cost {
			t.Errorf("returned %v != u.Cost %v", got, u.Cost)
		}
	})
}
