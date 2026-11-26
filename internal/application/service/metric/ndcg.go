package metric

import (
	"math"

	"github.com/Tencent/WeKnora/internal/types"
)

type NDCGMetric struct {
	k int
}

func NewNDCGMetric(k int) *NDCGMetric {
	return &NDCGMetric{k: k}
}

func (n *NDCGMetric) Compute(metricInput *types.MetricInput) float64 {
	gts := metricInput.RetrievalGT
	ids := metricInput.RetrievalIDs

	if len(ids) > n.k {
		ids = ids[:n.k]
	}

	gtSets := make(map[int]struct{}, len(gts))
	for _, gt := range gts {
		for _, g := range gt {
			gtSets[g] = struct{}{}
		}
	}

	relevanceScores := make(map[int]int)
	for _, docID := range ids {
		if _, exist := gtSets[docID]; exist {
			relevanceScores[docID] = 1
		} else {
			relevanceScores[docID] = 0
		}
	}

	var dcg float64
	countedRelevant := make(map[int]struct{}, len(gtSets))
	for i, docID := range ids {
		if _, exist := gtSets[docID]; !exist {
			continue
		}
		if _, seen := countedRelevant[docID]; seen {
			continue
		}
		countedRelevant[docID] = struct{}{}
		dcg += (math.Pow(2, float64(relevanceScores[docID])) - 1) / math.Log2(float64(i+2))
	}

	idealLen := min(len(gtSets), n.k)
	var idcg float64
	for i := 0; i < idealLen; i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}

	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}
