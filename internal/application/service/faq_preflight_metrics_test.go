package service

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestFAQPreflightMetricsEmptyAndEdgeCases(t *testing.T) {
	var nilMetrics *faqPreflightMetrics
	nilMetrics.Record("hit", "m1", 10*time.Millisecond)
	nilSnap := nilMetrics.Snapshot()
	if nilSnap.Total != 0 || len(nilSnap.ByOutcome) != 0 || len(nilSnap.Buckets) != 0 {
		t.Fatalf("expected empty snapshot from nil collector, got %+v", nilSnap)
	}
	nilMetrics.Reset()

	m := newFAQPreflightMetrics(0)
	if m.windowSize != defaultFAQPreflightWindowSize {
		t.Fatalf("expected default window size %d, got %d", defaultFAQPreflightWindowSize, m.windowSize)
	}
	emptySnap := m.Snapshot()
	if emptySnap.Total != 0 || len(emptySnap.ByOutcome) != 0 || len(emptySnap.Buckets) != 0 {
		t.Fatalf("expected empty snapshot, got %+v", emptySnap)
	}

	if got := percentileDuration(nil, 0.50); got != 0 {
		t.Fatalf("expected 0 for nil slice, got %v", got)
	}
	if got := percentileDuration([]time.Duration{}, 0.95); got != 0 {
		t.Fatalf("expected 0 for empty slice, got %v", got)
	}

	zeroBucket := newFAQPreflightBucket(0)
	zeroBucket.record(15 * time.Millisecond)
	zeroSnap := zeroBucket.snapshot(faqPreflightMetricKey{outcome: "hit", modelID: "m0"})
	if zeroSnap.Count != 1 || zeroSnap.TotalDuration != 15*time.Millisecond || zeroSnap.WindowSize != 0 || zeroSnap.P50 != 0 || zeroSnap.P95 != 0 {
		t.Fatalf("unexpected zero-window bucket snapshot: %+v", zeroSnap)
	}
}

func TestFAQPreflightMetricsPercentilesAndNormalization(t *testing.T) {
	m := newFAQPreflightMetrics(128)
	for i := 1; i <= 100; i++ {
		m.Record("exact_hit", "model-a", time.Duration(i)*time.Millisecond)
	}
	m.Record("  ", "", -5*time.Millisecond)

	snap := m.Snapshot()
	if snap.Total != 101 {
		t.Fatalf("expected total 101, got %d", snap.Total)
	}
	if snap.ByOutcome["exact_hit"] != 100 || snap.ByOutcome["unknown"] != 1 {
		t.Fatalf("unexpected outcome distribution: %+v", snap.ByOutcome)
	}
	if len(snap.Buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(snap.Buckets))
	}

	hitBucket := snap.Buckets[0]
	if hitBucket.Outcome != "exact_hit" || hitBucket.ModelID != "model-a" {
		t.Fatalf("unexpected first bucket key: %+v", hitBucket)
	}
	if hitBucket.Count != 100 || hitBucket.TotalDuration != 5050*time.Millisecond || hitBucket.WindowSize != 100 {
		t.Fatalf("unexpected hit bucket aggregates: %+v", hitBucket)
	}
	if hitBucket.P50 != 50*time.Millisecond || hitBucket.P95 != 95*time.Millisecond {
		t.Fatalf("expected P50=50ms P95=95ms, got P50=%v P95=%v", hitBucket.P50, hitBucket.P95)
	}

	unknownBucket := snap.Buckets[1]
	if unknownBucket.Outcome != "unknown" || unknownBucket.ModelID != "unknown" {
		t.Fatalf("expected normalized unknown labels, got %+v", unknownBucket)
	}
	if unknownBucket.Count != 1 || unknownBucket.TotalDuration != 0 || unknownBucket.P50 != 0 || unknownBucket.P95 != 0 {
		t.Fatalf("expected clamped zero duration, got %+v", unknownBucket)
	}
}

func TestFAQPreflightMetricsRingBufferWrap(t *testing.T) {
	m := newFAQPreflightMetrics(5)
	for i := 0; i < 95; i++ {
		m.Record("semantic_hit", "model-b", time.Second)
	}
	for _, d := range []time.Duration{
		50 * time.Millisecond,
		10 * time.Millisecond,
		40 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	} {
		m.Record("semantic_hit", "model-b", d)
	}

	snap := m.Snapshot()
	if snap.Total != 100 || len(snap.Buckets) != 1 {
		t.Fatalf("unexpected snapshot after wrap: %+v", snap)
	}
	b := snap.Buckets[0]
	if b.Count != 100 {
		t.Fatalf("expected count 100, got %d", b.Count)
	}
	if b.WindowSize != 5 {
		t.Fatalf("expected window size 5, got %d", b.WindowSize)
	}
	expectedTotal := 95*time.Second + 150*time.Millisecond
	if b.TotalDuration != expectedTotal {
		t.Fatalf("expected total duration %v, got %v", expectedTotal, b.TotalDuration)
	}
	if b.P50 != 30*time.Millisecond || b.P95 != 50*time.Millisecond {
		t.Fatalf("expected wrapped window P50=30ms P95=50ms, got P50=%v P95=%v", b.P50, b.P95)
	}
}

func TestFAQPreflightMetricsConcurrentRaceAndConsistency(t *testing.T) {
	const (
		windowSize       = 32
		writerGoroutines = 16
		recordsPerWriter = 300
	)
	outcomes := []string{"exact_hit", "semantic_hit", "miss", "timeout"}
	models := []string{"model-fast", "model-lite", "model-pro"}

	m := newFAQPreflightMetrics(windowSize)
	done := make(chan struct{})

	var readerWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				snap := m.Snapshot()
				var outcomeSum int64
				for _, c := range snap.ByOutcome {
					outcomeSum += c
				}
				var bucketSum int64
				for _, b := range snap.Buckets {
					bucketSum += b.Count
					if b.WindowSize < 0 || b.WindowSize > windowSize {
						t.Errorf("invalid window size: %d", b.WindowSize)
					}
					if b.P50 > b.P95 {
						t.Errorf("inconsistent percentiles: P50=%v > P95=%v", b.P50, b.P95)
					}
				}
				if snap.Total != outcomeSum || snap.Total != bucketSum {
					t.Errorf("inconsistent snapshot totals: total=%d outcomeSum=%d bucketSum=%d", snap.Total, outcomeSum, bucketSum)
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}

	var writerWG sync.WaitGroup
	for w := 0; w < writerGoroutines; w++ {
		writerWG.Add(1)
		go func(id int) {
			defer writerWG.Done()
			for i := 0; i < recordsPerWriter; i++ {
				outcome := outcomes[(id+i)%len(outcomes)]
				model := models[(id*3+i)%len(models)]
				elapsed := time.Duration((i%50)+1) * time.Millisecond
				m.Record(outcome, model, elapsed)
			}
		}(w)
	}

	writerWG.Wait()
	close(done)
	readerWG.Wait()

	finalSnap := m.Snapshot()
	expectedTotal := int64(writerGoroutines * recordsPerWriter)
	if finalSnap.Total != expectedTotal {
		t.Fatalf("expected final total %d, got %d", expectedTotal, finalSnap.Total)
	}
	if len(finalSnap.Buckets) != len(outcomes)*len(models) {
		t.Fatalf("expected %d buckets, got %d", len(outcomes)*len(models), len(finalSnap.Buckets))
	}
	var sum int64
	for _, b := range finalSnap.Buckets {
		sum += b.Count
		if b.WindowSize != windowSize {
			t.Fatalf("expected saturated window %d for %s/%s, got %d", windowSize, b.Outcome, b.ModelID, b.WindowSize)
		}
		if b.P50 <= 0 || b.P95 < b.P50 {
			t.Fatalf("unexpected percentiles for %s/%s: P50=%v P95=%v", b.Outcome, b.ModelID, b.P50, b.P95)
		}
	}
	if sum != expectedTotal {
		t.Fatalf("expected bucket sum %d, got %d", expectedTotal, sum)
	}
}

func TestFAQPreflightMetricsDefaultPackageFunctions(t *testing.T) {
	defaultFAQPreflightMetrics.Reset()
	t.Cleanup(func() {
		defaultFAQPreflightMetrics.Reset()
	})

	modelID := fmt.Sprintf("default-test-model-%d", time.Now().UnixNano())
	recordFAQPreflightMetric("hit", modelID, 25*time.Millisecond)
	recordFAQPreflightMetric("hit", modelID, 75*time.Millisecond)

	snap := snapshotFAQPreflightMetrics()
	if snap.Total != 2 || snap.ByOutcome["hit"] != 2 || len(snap.Buckets) != 1 {
		t.Fatalf("unexpected default snapshot: %+v", snap)
	}
	b := snap.Buckets[0]
	if b.ModelID != modelID || b.P50 != 25*time.Millisecond || b.P95 != 75*time.Millisecond {
		t.Fatalf("unexpected default bucket: %+v", b)
	}
}
