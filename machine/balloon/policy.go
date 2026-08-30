package balloon

import "github.com/clawkwork/clawk/machine/guest"

// Policy tracks swap demand for one VM. Each sample must be observed once.
type Policy struct{ trend SwapTrend }

func (p *Policy) Target(current, baseline, ceiling uint64, pressure int, s guest.Stats) uint64 {
	return MergedTarget(Pressure(pressure), current, baseline, ceiling, s, p.trend.Observe(s))
}
