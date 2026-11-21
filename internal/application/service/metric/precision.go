package metric

import (
	"github.com/Tencent/WeKnora/internal/types"
)

type PrecisionMetric struct{}

func NewPrecisionMetric() *PrecisionMetric {
	return &PrecisionMetric{}
}

func (r *PrecisionMetric) Compute(metricInput *types.MetricInput) float64 {
	gts := metricInput.RetrievalGT
	ids := metricInput.RetrievalIDs

	gtSets := SliceMap(gts, ToSet)

	if len(gts) == 0 {
		return 0.0
	}

	if len(ids) == 0 {
		return 0.0
	}

	var totalPrecision float64
	for _, gtSet := range gtSets {
		hits := Hit(ids, gtSet)
		totalPrecision += float64(hits) / float64(len(ids))
	}

	return totalPrecision / float64(len(gts))
}
