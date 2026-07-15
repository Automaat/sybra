package loadscale

import (
	"math"
	"runtime"
	"time"
)

// ScaleDuration multiplies base by factor. Factors below 1 are treated as 1
// so bad caller input cannot shorten a test deadline.
func ScaleDuration(base time.Duration, factor int64) time.Duration {
	if factor < 1 {
		factor = 1
	}
	return time.Duration(int64(base) * factor)
}

// HostOversubscriptionFactor returns an integer multiplier based on the
// host's 1-minute load average per CPU, capped at ceiling.
func HostOversubscriptionFactor(ceiling int64) int64 {
	load, ok := HostLoadPerCPU()
	return OversubscriptionFactor(load, ok, ceiling)
}

// OversubscriptionFactor converts a normalized load average into a deadline
// multiplier. It fails safe toward no scaling when the load cannot be read.
func OversubscriptionFactor(load float64, ok bool, ceiling int64) int64 {
	if ceiling < 1 {
		ceiling = 1
	}
	if !ok || load <= 1 {
		return 1
	}
	factor := int64(math.Ceil(load))
	if factor > ceiling {
		return ceiling
	}
	return factor
}

// LoadPerCPU divides a 1-minute load average by CPU count.
func LoadPerCPU(load1 float64) (float64, bool) {
	cpus := effectiveCPUCount()
	if cpus <= 0 {
		return 0, false
	}
	return load1 / float64(cpus), true
}

func effectiveCPUCount() int {
	cpus := runtime.NumCPU()
	if gomax := runtime.GOMAXPROCS(0); gomax > 0 && (cpus <= 0 || gomax < cpus) {
		return gomax
	}
	return cpus
}
