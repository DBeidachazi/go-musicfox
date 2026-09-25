package automix

import (
	"math"
	"sort"
)

func sortFloats(v []float64) { sort.Float64s(v) }

// round2 与上游 round 一致：保留两位小数。
func round2(seconds float64) float64 { return math.Round(seconds*100) / 100 }

func ptr[T any](v T) *T { return &v }
