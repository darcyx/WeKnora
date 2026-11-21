package metric

import (
	"github.com/Tencent/WeKnora/internal/types"
)

type MRRMetric struct{}

func NewMRRMetric() *MRRMetric {
	return &MRRMetric{}
}

func (m *MRRMetric) Compute(metricInput *types.MetricInput) float64 {
	gts := metricInput.RetrievalGT
	ids := metricInput.RetrievalIDs

	gtSets := make([]map[int]struct{}, len(gts))
	for i, gt := range gts {
		gtSets[i] = make(map[int]struct{})
		for _, docID := range gt {
			gtSets[i][docID] = struct{}{}
		}
	}

	var sumRR float64
	for _, gtSet := range gtSets {
		for i, predID := range ids {
			if _, ok := gtSet[predID]; ok {
				sumRR += 1.0 / float64(i+1)
				break
			}
		}
	}
	if len(gtSets) == 0 {
		return 0
	}
	return sumRR / float64(len(gtSets))
}
