//go:build !linux && !darwin

package pressure

import "math"

func readSample(string) Sample {
	return Sample{
		DiskFreePct:     math.NaN(),
		MemAvailablePct: math.NaN(),
		LoadPerCPU:      math.NaN(),
	}
}
