package main

import (
	"math"
	"sort"
)

// windowSpec maps a UI window to a duration and a series bucket size.
type windowSpec struct {
	hours     int
	bucketMin int // bucket width in minutes
}

var windows = map[string]windowSpec{
	"24h": {hours: 24, bucketMin: 60},
	"7d":  {hours: 24 * 7, bucketMin: 360},
	"30d": {hours: 24 * 30, bucketMin: 1440},
}

// percentile takes p in [0,1] over a sorted ascending slice (linear interpolation).
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := p * float64(n-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	w := idx - float64(lo)
	return sorted[lo]*(1-w) + sorted[hi]*w
}

// bucketMedians groups samples by fixed-width buckets anchored at sinceMs and returns
// (bucketStartMs, median) pairs in ascending order. buckets are width bucketMs.
func bucketMedians(samples []sample, sinceMs, bucketMs int64) []seriesPoint {
	if len(samples) == 0 || bucketMs <= 0 {
		return nil
	}
	byBucket := map[int64][]float64{}
	for _, s := range samples {
		if s.tps <= 0 {
			continue
		}
		b := ((s.ts-sinceMs)/bucketMs)*bucketMs + sinceMs
		byBucket[b] = append(byBucket[b], s.tps)
	}
	keys := make([]int64, 0, len(byBucket))
	for k := range byBucket {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]seriesPoint, 0, len(keys))
	for _, k := range keys {
		vs := byBucket[k]
		sort.Float64s(vs)
		out = append(out, seriesPoint{B: k / 1000, V: round1(percentile(vs, 0.5))})
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func roundInt(v float64) int { return int(math.Round(v)) }
