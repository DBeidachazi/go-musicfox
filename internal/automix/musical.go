package automix

import "math"

// BeatsPerPhrase 一个乐句：四小节四拍。
const BeatsPerPhrase = BeatsPerBar * 4

// TempoRelation 两个速度的关系，对应不同动作。
type TempoRelation string

const (
	TempoLocked      TempoRelation = "locked"
	TempoNear        TempoRelation = "near"
	TempoStretchable TempoRelation = "stretchable"
	TempoDrifting    TempoRelation = "drifting"
	TempoFar         TempoRelation = "far"
	TempoUnknown     TempoRelation = "unknown"
)

const (
	lockedDeviation = 0.015
	// RawRateLimit 同一速度测两次与两个不同速度的分界。
	RawRateLimit = 0.0594
	stretchLimit = 0.12
	driftLimit   = 0.25
)

// TempoMatch 速度匹配结果。Stretch 是出场曲应以多少倍速运行。
type TempoMatch struct {
	Relation TempoRelation
	Stretch  float64
	Ratio    float64
}

// foldRatio 半速与倍速是同一个速度。
func foldRatio(ratio float64) float64 {
	for ratio > math.Sqrt2 {
		ratio /= 2
	}
	for ratio < 1/math.Sqrt2 {
		ratio *= 2
	}
	return ratio
}

func positive(v *float64) (float64, bool) {
	if v == nil || !(*v > 0) {
		return 0, false
	}
	return *v, true
}

// MatchTempo 对应上游 tempoMatch。
func MatchTempo(fromBPM, toBPM *float64) TempoMatch {
	from, ok1 := positive(fromBPM)
	to, ok2 := positive(toBPM)
	if !ok1 || !ok2 {
		return TempoMatch{Relation: TempoUnknown, Stretch: 1, Ratio: 1}
	}
	ratio := foldRatio(to / from)
	dev := math.Abs(ratio - 1)
	switch {
	case dev < lockedDeviation:
		return TempoMatch{TempoLocked, 1, ratio}
	case dev <= RawRateLimit:
		return TempoMatch{TempoNear, ratio, ratio}
	case dev <= stretchLimit:
		return TempoMatch{TempoStretchable, ratio, ratio}
	case dev <= driftLimit:
		return TempoMatch{TempoDrifting, 1, ratio}
	default:
		return TempoMatch{TempoFar, 1, ratio}
	}
}

// SettledBPM 曲尾的速度；全曲与尾段两次测量对不上时答"不知道"。
func SettledBPM(whole, outro *float64) *float64 {
	w, wok := positive(whole)
	o, ook := positive(outro)
	if !ook {
		if wok {
			return ptr(w)
		}
		return nil
	}
	if !wok {
		return ptr(o)
	}
	folded := foldRatio(o / w)
	if math.Abs(folded-1) > RawRateLimit {
		return nil
	}
	return ptr(w * folded)
}

// TempoLengthScale 速度关系对过渡长度的系数。
var TempoLengthScale = map[TempoRelation]float64{
	TempoLocked:      1.3,
	TempoNear:        1.2,
	TempoStretchable: 1.1,
	TempoDrifting:    0.6,
	TempoFar:         0.5,
	TempoUnknown:     1,
}

// QuantiseToMusic 把长度取整到乐句 / 小节 / 拍，超出上限时整单位往下退。
func QuantiseToMusic(seconds float64, beatSec *float64, maxSeconds float64) float64 {
	if beatSec == nil || !(*beatSec > 0) {
		return math.Min(seconds, maxSeconds)
	}
	b := *beatSec
	unitFor := func(beats float64) float64 {
		switch {
		case beats >= BeatsPerPhrase:
			return BeatsPerPhrase
		case beats >= BeatsPerBar:
			return BeatsPerBar
		default:
			return 1
		}
	}
	wanted := seconds / b
	unit := unitFor(wanted)
	beats := math.Round(wanted/unit) * unit
	for beats*b > maxSeconds && beats > 1 {
		if next := unitFor(beats - unit); next < unit {
			unit = next
		}
		beats = math.Max(1, beats-unit)
	}
	return math.Max(1, beats) * b
}

// Grid 一条等距网格。
type Grid struct {
	Offset float64
	Period float64
}

// BarGrid 由速度与重拍位置得出网格；beatsPerBar=1 时即拍网格。
func BarGrid(bpm *float64, downbeatOffset *float64, beatsPerBar int) *Grid {
	v, ok := positive(bpm)
	if !ok || downbeatOffset == nil {
		return nil
	}
	period := 60 / v * float64(beatsPerBar)
	return &Grid{Offset: modulo(*downbeatOffset, period), Period: period}
}

// SnapToGrid 在容差内吸附到最近的网格线。
func SnapToGrid(seconds float64, grid *Grid, tolerance float64) float64 {
	if grid == nil || !(grid.Period > 0) || !(tolerance > 0) {
		return seconds
	}
	line := grid.Offset + math.Round((seconds-grid.Offset)/grid.Period)*grid.Period
	if math.Abs(line-seconds) <= tolerance && line > 0 {
		return line
	}
	return seconds
}

// AlignEntry 进场曲从哪里起播，才能让它的网格线落在出场曲的网格线上。永不早于 earliest。
func AlignEntry(earliest float64, incoming *Grid, outgoingBarPhase *float64) float64 {
	if incoming == nil || outgoingBarPhase == nil || !(incoming.Period > 0) {
		return earliest
	}
	barsIn := math.Ceil((earliest - incoming.Offset) / incoming.Period)
	line := incoming.Offset + math.Max(0, barsIn)*incoming.Period
	entry := line - *outgoingBarPhase
	if entry >= earliest {
		return entry
	}
	return entry + incoming.Period
}
