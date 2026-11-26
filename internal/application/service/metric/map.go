package metric

import (
	"github.com/Tencent/WeKnora/internal/types"
)

type MAPMetric struct{}

func NewMAPMetric() *MAPMetric {
	return &MAPMetric{}
}

func (m *MAPMetric) Compute(metricInput *types.MetricInput) float64 {
	gts := metricInput.RetrievalGT
	ids := metricInput.RetrievalIDs

	gtSets := make([]map[int]struct{}, len(gts))
	for i, gt := range gts {
		gtSets[i] = make(map[int]struct{})
		for _, docID := range gt {
			gtSets[i][docID] = struct{}{}
		}
	}

	var apSum float64

	for _, gtSet := range gtSets {
		relevantCount := len(gtSet)
		predHits := make([]bool, len(ids))
		for i, predID := range ids {
			if _, ok := gtSet[predID]; ok {
				predHits[i] = true
				delete(gtSet, predID)
			} else {
				predHits[i] = false
			}
		}

		var (
			ap       float64
			hitCount int
		)

		for k := 0; k < len(predHits); k++ {
			if predHits[k] {
				hitCount++
				ap += float64(hitCount) / float64(k+1)
			}
		}
		if hitCount > 0 {
			ap /= float64(relevantCount)
		}
		apSum += ap
	}

	if len(gtSets) == 0 {
		return 0
	}
	return apSum / float64(len(gtSets))
}
