package metric

import (
	"github.com/Tencent/WeKnora/internal/types"
)

type RecallMetric struct{}

func NewRecallMetric() *RecallMetric {
	return &RecallMetric{}
}

func (r *RecallMetric) Compute(metricInput *types.MetricInput) float64 {
	gts := metricInput.RetrievalGT
	ids := metricInput.RetrievalIDs

	gtSets := SliceMap(gts, ToSet)
	if len(gtSets) == 0 {
		return 0.0
	}

	if len(ids) == 0 {
		return 0.0
	}

	var totalRecall float64
	for _, gtSet := range gtSets {
		hits := Hit(ids, gtSet)
		if len(gtSet) > 0 {
			totalRecall += float64(hits) / float64(len(gtSet))
		}
	}

	return totalRecall / float64(len(gtSets))
}
