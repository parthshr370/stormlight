package types

// ModelCost is per-token pricing, in **USD per MILLION tokens**. There is
// deliberately no separate one-hour cache-write price: those writes cost
// input * 2 (see CalculateCost).
type ModelCost struct {
	Input      float64 `json:"input"`      // $/million tokens
	Output     float64 `json:"output"`     // $/million tokens
	CacheRead  float64 `json:"cacheRead"`  // $/million tokens
	CacheWrite float64 `json:"cacheWrite"` // $/million tokens
}

// Cost is the computed per-component + total cost of a Usage (USD).
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Usage is token accounting for a turn. Optional fields CacheWrite1h / Reasoning
// are pointers so "absent" is distinguishable from 0. CacheWrite1h is the
// subset of CacheWrite written with one-hour retention; Reasoning is a subset
// of Output.
type Usage struct {
	Input        int  `json:"input"`
	Output       int  `json:"output"`
	CacheRead    int  `json:"cacheRead"`
	CacheWrite   int  `json:"cacheWrite"`
	CacheWrite1h *int `json:"cacheWrite1h,omitempty"`
	Reasoning    *int `json:"reasoning,omitempty"`
	TotalTokens  int  `json:"totalTokens"`
	Cost         Cost `json:"cost"`
}

// CalculateCost fills u.Cost from the model's per-million pricing. One-hour
// cache writes are billed at 2× the base input rate (not a separate
// cache-write price); short writes use the cacheWrite rate. Mutates u in place
// and returns the computed cost.
func CalculateCost(mc ModelCost, u *Usage) Cost {
	longWrite := 0
	if u.CacheWrite1h != nil {
		longWrite = *u.CacheWrite1h
	}
	shortWrite := u.CacheWrite - longWrite

	c := Cost{
		Input:      (mc.Input / 1_000_000) * float64(u.Input),
		Output:     (mc.Output / 1_000_000) * float64(u.Output),
		CacheRead:  (mc.CacheRead / 1_000_000) * float64(u.CacheRead),
		CacheWrite: (mc.CacheWrite*float64(shortWrite) + mc.Input*2*float64(longWrite)) / 1_000_000,
	}
	c.Total = c.Input + c.Output + c.CacheRead + c.CacheWrite
	u.Cost = c
	return c
}
