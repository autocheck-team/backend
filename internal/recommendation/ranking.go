package recommendation

import (
	"bytes"
	"math"
	"slices"
)

func validVector(vector []float64) bool {
	if len(vector) == 0 {
		return false
	}
	nonzero := false
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxFloat32 {
			return false
		}
		nonzero = nonzero || float32(value) != 0
	}
	return nonzero
}

func cosine(left, right []float64) (float64, bool) {
	if len(left) != len(right) || !validVector(left) || !validVector(right) {
		return 0, false
	}
	var leftNorm, rightNorm float64
	for i := range left {
		leftNorm = math.Hypot(leftNorm, left[i])
		rightNorm = math.Hypot(rightNorm, right[i])
	}
	var score float64
	for i := range left {
		score += (left[i] / leftNorm) * (right[i] / rightNorm)
	}
	return max(-1, min(1, score)), true
}

func rank(items []candidate, query []float64, limit int) ([]match, bool) {
	matches := make([]match, 0, len(items))
	for _, item := range items {
		score, ok := cosine(item.Vector, query)
		if !ok {
			return nil, false
		}
		matches = append(matches, match{ID: item.ID, Score: score})
	}
	slices.SortFunc(matches, func(left, right match) int {
		if left.Score > right.Score {
			return -1
		}
		if left.Score < right.Score {
			return 1
		}
		return bytes.Compare(left.ID[:], right.ID[:])
	})
	return matches[:min(limit, len(matches))], true
}
