package service

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultFAQPreflightWindowSize = 256

type faqPreflightMetricKey struct {
	outcome string
	modelID string
}

type FAQPreflightBucketSnapshot struct {
	Outcome       string        `json:"outcome"`
	ModelID       string        `json:"model_id"`
	Count         int64         `json:"count"`
	TotalDuration time.Duration `json:"total_duration"`
	WindowSize    int           `json:"window_size"`
	P50           time.Duration `json:"p50"`
	P95           time.Duration `json:"p95"`
}

type FAQPreflightMetricsSnapshot struct {
	Total     int64                        `json:"total"`
	ByOutcome map[string]int64             `json:"by_outcome"`
	Buckets   []FAQPreflightBucketSnapshot `json:"buckets"`
}

type faqPreflightBucket struct {
	mu      sync.Mutex
	count   int64
	total   time.Duration
	samples []time.Duration
	next    int
	filled  int
}

func newFAQPreflightBucket(windowSize int) *faqPreflightBucket {
	if windowSize < 0 {
		windowSize = 0
	}
	return &faqPreflightBucket{
		samples: make([]time.Duration, windowSize),
	}
}

func (b *faqPreflightBucket) record(elapsed time.Duration) {
	b.mu.Lock()
	b.count++
	b.total += elapsed
	if len(b.samples) > 0 {
		b.samples[b.next] = elapsed
		b.next = (b.next + 1) % len(b.samples)
		if b.filled < len(b.samples) {
			b.filled++
		}
	}
	b.mu.Unlock()
}

func (b *faqPreflightBucket) snapshot(key faqPreflightMetricKey) FAQPreflightBucketSnapshot {
	b.mu.Lock()
	count := b.count
	total := b.total
	filled := b.filled
	var copied []time.Duration
	if filled > 0 {
		copied = make([]time.Duration, filled)
		copy(copied, b.samples[:filled])
	}
	b.mu.Unlock()

	if len(copied) > 1 {
		sort.Slice(copied, func(i, j int) bool {
			return copied[i] < copied[j]
		})
	}

	return FAQPreflightBucketSnapshot{
		Outcome:       key.outcome,
		ModelID:       key.modelID,
		Count:         count,
		TotalDuration: total,
		WindowSize:    filled,
		P50:           percentileDuration(copied, 0.50),
		P95:           percentileDuration(copied, 0.95),
	}
}

type faqPreflightMetrics struct {
	windowSize int
	buckets    sync.Map
}

func newFAQPreflightMetrics(windowSize int) *faqPreflightMetrics {
	if windowSize <= 0 {
		windowSize = defaultFAQPreflightWindowSize
	}
	return &faqPreflightMetrics{
		windowSize: windowSize,
	}
}

func (m *faqPreflightMetrics) Record(outcome, modelID string, elapsed time.Duration) {
	if m == nil {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	key := faqPreflightMetricKey{
		outcome: normalizeFAQPreflightLabel(outcome),
		modelID: normalizeFAQPreflightLabel(modelID),
	}
	var bucket *faqPreflightBucket
	if existing, ok := m.buckets.Load(key); ok {
		bucket = existing.(*faqPreflightBucket)
	} else {
		actual, _ := m.buckets.LoadOrStore(key, newFAQPreflightBucket(m.windowSize))
		bucket = actual.(*faqPreflightBucket)
	}
	bucket.record(elapsed)
}

func (m *faqPreflightMetrics) Snapshot() FAQPreflightMetricsSnapshot {
	snap := FAQPreflightMetricsSnapshot{
		ByOutcome: make(map[string]int64),
		Buckets:   make([]FAQPreflightBucketSnapshot, 0),
	}
	if m == nil {
		return snap
	}

	m.buckets.Range(func(k, v any) bool {
		key := k.(faqPreflightMetricKey)
		bucket := v.(*faqPreflightBucket)
		bs := bucket.snapshot(key)
		snap.Total += bs.Count
		snap.ByOutcome[bs.Outcome] += bs.Count
		snap.Buckets = append(snap.Buckets, bs)
		return true
	})

	sort.Slice(snap.Buckets, func(i, j int) bool {
		if snap.Buckets[i].Outcome != snap.Buckets[j].Outcome {
			return snap.Buckets[i].Outcome < snap.Buckets[j].Outcome
		}
		return snap.Buckets[i].ModelID < snap.Buckets[j].ModelID
	})

	return snap
}

func (m *faqPreflightMetrics) Reset() {
	if m == nil {
		return
	}
	m.buckets.Range(func(k, _ any) bool {
		m.buckets.Delete(k)
		return true
	})
}

func normalizeFAQPreflightLabel(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func percentileDuration(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 || p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

var defaultFAQPreflightMetrics = newFAQPreflightMetrics(defaultFAQPreflightWindowSize)

func recordFAQPreflightMetric(outcome, modelID string, elapsed time.Duration) {
	defaultFAQPreflightMetrics.Record(outcome, modelID, elapsed)
}

func snapshotFAQPreflightMetrics() FAQPreflightMetricsSnapshot {
	return defaultFAQPreflightMetrics.Snapshot()
}
