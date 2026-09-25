package automix

import (
	"math"
	"sort"
)

// BeatGrid 拍点模型（Beat This!）的输出：全部拍与其中的重拍，单位秒。
type BeatGrid struct {
	Beats     []float64
	Downbeats []float64
}

// GridFields 由拍点折算出的六个档案字段，有模型时整体替换自相关估计。
type GridFields struct {
	BPM                *float64
	OutroBPM           *float64
	BeatOffset         float64
	DownbeatOffset     *float64
	HeadDownbeatOffset *float64
	BeatsPerBar        int
}

const (
	gridOutroWindowSec = 30
	gridMinBeats       = 8
)

// medianAvg 偶数个时取中间两数均值（与上游 beatThis.ts 的 median 一致）。
func medianAvg(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	m := len(s) >> 1
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func intervals(times []float64) []float64 {
	out := make([]float64, 0, len(times))
	for i := 1; i < len(times); i++ {
		out = append(out, times[i]-times[i-1])
	}
	return out
}

// GridFromBeats 把拍点折算成 {offset, period} 网格。拍太少时返回 nil。
func GridFromBeats(grid BeatGrid, partial bool) *GridFields {
	if len(grid.Beats) < gridMinBeats {
		return nil
	}
	period := medianAvg(intervals(grid.Beats))
	if math.IsNaN(period) || period <= 0 {
		return nil
	}
	last := grid.Beats[len(grid.Beats)-1]
	var spans []float64
	for _, s := range intervals(grid.Downbeats) {
		if n := math.Round(s / period); n >= 2 && n <= 12 {
			spans = append(spans, n)
		}
	}
	beatsPerBar := 4
	if perBar := math.Round(medianAvg(spans)); !math.IsNaN(perBar) && perBar >= 2 {
		beatsPerBar = int(perBar)
	}
	barSec := period * float64(beatsPerBar)

	var outroBeats []float64
	for _, b := range grid.Beats {
		if b >= last-gridOutroWindowSec {
			outroBeats = append(outroBeats, b)
		}
	}
	f := &GridFields{
		BPM:         ptr(60 / period),
		BeatOffset:  modulo(last, period),
		BeatsPerBar: beatsPerBar,
	}
	if !partial && len(outroBeats) >= gridMinBeats {
		if op := medianAvg(intervals(outroBeats)); op > 0 {
			f.OutroBPM = ptr(60 / op)
		}
	}
	if n := len(grid.Downbeats); n > 0 {
		f.DownbeatOffset = ptr(modulo(grid.Downbeats[n-1], barSec))
		f.HeadDownbeatOffset = ptr(modulo(grid.Downbeats[0], barSec))
	}
	return f
}
