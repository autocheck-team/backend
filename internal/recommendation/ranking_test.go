package recommendation

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestCosine(t *testing.T) {
	for _, test := range []struct {
		name        string
		left, right []float64
		score       float64
		valid       bool
	}{
		{"same", []float64{3, 4}, []float64{6, 8}, 1, true},
		{"orthogonal", []float64{1, 0}, []float64{0, 1}, 0, true},
		{"opposite", []float64{1, 0}, []float64{-2, 0}, -1, true},
		{"zero", []float64{0, 0}, []float64{1, 0}, 0, false},
		{"empty", nil, nil, 0, false},
		{"dimensions", []float64{1}, []float64{1, 2}, 0, false},
		{"nan", []float64{math.NaN()}, []float64{1}, 0, false},
		{"infinity", []float64{math.Inf(1)}, []float64{1}, 0, false},
		{"large", []float64{math.MaxFloat32, math.MaxFloat32}, []float64{1, 1}, 1, true},
		{"underflow", []float64{math.SmallestNonzeroFloat64}, []float64{1}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// arrange
			t.Parallel()
			// act
			score, ok := cosine(test.left, test.right)
			// assert
			if ok != test.valid || math.Abs(score-test.score) > 1e-12 {
				t.Fatalf("score=%v valid=%v", score, ok)
			}
		})
	}
}

func TestRank(t *testing.T) {
	// arrange
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	items := []candidate{{ID: second, Vector: []float64{1, 0}}, {ID: first, Vector: []float64{2, 0}}, {ID: uuid.New(), Vector: []float64{0, 1}}}
	// act
	result, ok := rank(items, []float64{1, 0}, 2)
	// assert
	if !ok || len(result) != 2 || result[0].ID != first || result[1].ID != second {
		t.Fatalf("ranking=%v", result)
	}
	if _, ok := rank(items, []float64{1}, 10); ok {
		t.Fatal("incompatible vectors ranked")
	}
}
