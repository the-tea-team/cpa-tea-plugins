package main

import (
	"math"
	"testing"
)

func TestPercentileBasics(t *testing.T) {
	vs := []float64{100, 200, 300, 400, 500}
	if got := percentile(vs, 0.5); got != 300 {
		t.Fatalf("p50 = %v, want 300", got)
	}
	if got := percentile(vs, 0.1); got != 140 {
		t.Fatalf("p10 = %v, want 140 (interpolated)", got)
	}
	if got := percentile([]float64{}, 0.5); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
	if got := percentile([]float64{42}, 0.9); got != 42 {
		t.Fatalf("single = %v, want 42", got)
	}
}

func TestPercentileOrdering(t *testing.T) {
	vs := []float64{232, 298, 355, 448, 510, 720, 950}
	p10, p50, p90 := percentile(vs, 0.1), percentile(vs, 0.5), percentile(vs, 0.9)
	if !(p10 < p50 && p50 < p90) {
		t.Fatalf("expected p10 < p50 < p90, got %v %v %v", p10, p50, p90)
	}
}

func TestBucketMedians(t *testing.T) {
	base := int64(1_700_000_000_000)
	bucket := int64(3_600_000)
	ss := []sample{
		{ts: base + 1000, tps: 300},
		{ts: base + 2000, tps: 310},
		{ts: base + bucket + 500, tps: 500},
		{ts: base + bucket + 600, tps: 520},
		{ts: base + bucket + 700, tps: 510},
		{ts: base + 999, tps: 0}, // invalid samples skipped
	}
	got := bucketMedians(ss, base, bucket)
	if len(got) != 2 {
		t.Fatalf("want 2 buckets, got %d", len(got))
	}
	if math.Abs(got[0].V-305) > 0.001 {
		t.Fatalf("bucket0 median = %v, want 305", got[0].V)
	}
	if math.Abs(got[1].V-510) > 0.001 {
		t.Fatalf("bucket1 median = %v, want 510", got[1].V)
	}
	if got[0].B != base/1000 || got[1].B != (base+bucket)/1000 {
		t.Fatalf("bucket starts wrong: %v %v", got[0].B, got[1].B)
	}
}

func TestWindowSpecs(t *testing.T) {
	for _, w := range []string{"24h", "7d", "30d"} {
		spec, ok := windows[w]
		if !ok || spec.hours <= 0 || spec.bucketMin <= 0 {
			t.Fatalf("window %s missing or invalid", w)
		}
		if spec.hours*60/spec.bucketMin > 40 {
			t.Fatalf("window %s yields too many buckets (%d)", w, spec.hours*60/spec.bucketMin)
		}
	}
}
