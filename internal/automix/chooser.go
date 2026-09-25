package automix

import (
	"fmt"
	"math"
)

// TransitionStyle 四种接法。
type TransitionStyle string

const (
	// StyleBeatCut 进场曲一开始就是满电平：出场曲被切断。
	StyleBeatCut TransitionStyle = "beatCut"
	// StyleBassSwap 重叠，但低频同一时刻只属于一方。
	StyleBassSwap TransitionStyle = "bassSwap"
	// StyleTailRide 长重叠，进场曲从衰减尾巴底下浮上来。
	StyleTailRide TransitionStyle = "tailRide"
	// StylePlainBlend 等功率交叉淡化，什么都不知道时的兜底。
	StylePlainBlend TransitionStyle = "plainBlend"
)

// KeyRelation 调性关系。
type KeyRelation string

const (
	KeyCompatible KeyRelation = "compatible"
	KeyAdjacent   KeyRelation = "adjacent"
	KeyNeutral    KeyRelation = "neutral"
	KeyClashing   KeyRelation = "clashing"
	KeyUnknown    KeyRelation = "unknown"
)

const (
	minKeyConfidence     = 0.25
	fadeOutSlopeDBPerSec = -1.5
	introSilenceSec      = 0.3
	// BeatCutSec 切口也不是瞬时的。
	BeatCutSec = 0.04
	// CutLeadSec 一次切可占用出场曲多少。
	CutLeadSec = 1.5
	// HeadBudgetSec 等待时最多吞掉进场曲自身开头多少。
	HeadBudgetSec       = 0.05
	energyStepCeilingDB = 12.0
	energyStepScale     = 0.4
	hotStartScale       = 0.6
)

var keyLengthScale = map[KeyRelation]float64{
	KeyCompatible: 1, KeyAdjacent: 0.85, KeyNeutral: 0.7, KeyClashing: 0.4, KeyUnknown: 1,
}

var styleScale = map[TransitionStyle]float64{
	StyleBeatCut: 1, StyleBassSwap: 1, StyleTailRide: 1.4, StylePlainBlend: 1,
}

func usableKey(e *KeyEstimate) *KeyEstimate {
	if e != nil && e.Key >= 0 && e.Confidence >= minKeyConfidence {
		return e
	}
	return nil
}

func endKey(p *TrackProfile, outro bool) *KeyEstimate {
	if p == nil {
		return nil
	}
	endpoint := &p.IntroKey
	if outro {
		endpoint = p.OutroKey
	}
	if k := usableKey(endpoint); k != nil {
		return k
	}
	return usableKey(&KeyEstimate{Key: p.Key, Major: p.Major, Confidence: p.KeyConfidence})
}

// RelateKeys 出场曲结尾与进场曲开头的调性关系。只用来挑接法，不用来改音高。
func RelateKeys(from, to *TrackProfile) KeyRelation {
	a, b := endKey(from, true), endKey(to, false)
	if a == nil || b == nil {
		return KeyUnknown
	}
	interval := ((b.Key-a.Key)%12 + 12) % 12
	if a.Major == b.Major {
		if interval == 0 || interval == 7 || interval == 5 {
			return KeyCompatible
		}
		if interval == 2 || interval == 10 {
			return KeyAdjacent
		}
	} else if (a.Major && interval == 9) || (!a.Major && interval == 3) {
		return KeyCompatible
	} else if interval == 0 {
		return KeyAdjacent
	}
	if interval == 1 || interval == 11 || interval == 6 {
		return KeyClashing
	}
	return KeyNeutral
}

// StyleChoice 接法选择结果。
type StyleChoice struct {
	Style       TransitionStyle
	Relation    KeyRelation
	Tempo       TempoMatch
	LengthScale float64
	TiltDB      [2]float64
	EchoThrow   bool
	Reason      string
}

// ChooseTransitionStyle 按规则顺序挑接法，最无缝的在前。
// 启发式只决定"怎么接"，不决定"接不接"。
func ChooseTransitionStyle(from, to *TrackProfile) StyleChoice {
	relation := RelateKeys(from, to)
	var fromBPM, fromOutro, toBPM *float64
	if from != nil {
		fromBPM, fromOutro = from.BPM, from.OutroBPM
	}
	if to != nil {
		toBPM = to.BPM
	}
	tempo := MatchTempo(SettledBPM(fromBPM, fromOutro), toBPM)
	var tilt [2]float64
	if from != nil && to != nil {
		tilt = ToneTilt(from.OutroTone, to.IntroTone)
	}
	step := 0.0
	if from != nil && from.TailDB != nil && to != nil {
		step = math.Min(energyStepCeilingDB, math.Abs(*from.TailDB-to.HeadDB))
	}
	energyScale := 1 + step/energyStepCeilingDB*energyStepScale

	decide := func(style TransitionStyle, reason string, extra float64) StyleChoice {
		es := energyScale
		if style == StyleBeatCut {
			es = 1
		}
		c := StyleChoice{
			Style:    style,
			Relation: relation,
			Tempo:    tempo,
			// 各系数单独看都对，相乘可能变成一次没人写下来的硬切，故设 0.25 地板。
			LengthScale: math.Max(0.25, keyLengthScale[relation]*styleScale[style]*extra*
				TempoLengthScale[tempo.Relation]*es),
			TiltDB:    tilt,
			EchoThrow: style == StyleBeatCut || (from != nil && from.EndsHot != nil && *from.EndsHot),
			Reason:    reason,
		}
		if style == StyleBeatCut || style == StylePlainBlend {
			c.TiltDB = [2]float64{}
		}
		return c
	}

	if to != nil && to.StartsHot && from != nil && from.BPM != nil && *from.BPM > 0 {
		if to.LeadIn >= 60 / *from.BPM {
			return decide(StyleBeatCut, "the next track starts at full level, so this one is cut", 1)
		}
		return decide(StyleBassSwap, "the next track starts at full level, so the overlap is kept short", hotStartScale)
	}
	if tempo.Relation == TempoFar && from != nil && from.BPM != nil && *from.BPM > 0 && to != nil && to.LeadIn >= 60 / *from.BPM {
		return decide(StyleBeatCut, fmt.Sprintf("the two tempos are %d%% apart, so this one is cut",
			int(math.Round(math.Abs(tempo.Ratio-1)*100))), 1)
	}
	if from != nil && from.OutroSlope != nil && *from.OutroSlope <= fadeOutSlopeDBPerSec {
		return decide(StyleBassSwap, "this track fades out on its own", 1)
	}
	if from != nil && from.EndsHot != nil && !*from.EndsHot && to != nil && (to.LeadIn > introSilenceSec || !to.StartsHot) {
		return decide(StyleTailRide, "a decaying tail with an intro to come up underneath it", 1)
	}
	if from != nil || to != nil {
		return decide(StyleBassSwap, "overlapped, with the low end handed over", 1)
	}
	return decide(StylePlainBlend, "nothing measured about either track", 1)
}

// BlendShapeRequest ShapeBlend 的输入。
type BlendShapeRequest struct {
	Style          TransitionStyle
	Room           float64
	Overlap        float64
	Crossover      float64
	NextBeatIn     *float64
	PeriodSec      *float64
	IncomingLeadIn *float64
}

// StyledBlend 接法落到时间轴上：先等 Hold 秒，再用 Overlap 秒交接，在 Crossover 处换位。
type StyledBlend struct {
	Style      TransitionStyle
	Hold       float64
	Overlap    float64
	Crossover  float64
	ShapeBands bool
	SweepOut   bool
	// Together 两曲保持同一电平的中段占比——混音与淡化的区别。
	Together float64
}

var together = map[TransitionStyle]float64{
	StyleBeatCut: 0, StyleBassSwap: 0.55, StyleTailRide: 0.5, StylePlainBlend: 0,
}

// ShapeBlend 把接法变成排程。
func ShapeBlend(r BlendShapeRequest) StyledBlend {
	if r.Style == StyleBeatCut {
		leadIn := 0.0
		if r.IncomingLeadIn != nil {
			leadIn = *r.IncomingLeadIn
		}
		maxHold := math.Min(r.Room-BeatCutSec, leadIn+HeadBudgetSec)
		hold := 0.0
		if r.PeriodSec != nil && r.NextBeatIn != nil && *r.PeriodSec > 0 {
			if steps := math.Floor((maxHold - *r.NextBeatIn) / *r.PeriodSec); steps >= 0 {
				hold = *r.NextBeatIn + steps**r.PeriodSec
			}
		}
		return StyledBlend{Style: r.Style, Hold: math.Max(0, hold), Overlap: BeatCutSec, Crossover: 0.5}
	}
	return StyledBlend{
		Style:      r.Style,
		Overlap:    r.Overlap,
		Crossover:  r.Crossover,
		Together:   together[r.Style],
		ShapeBands: r.Style != StylePlainBlend,
		SweepOut:   r.Style == StyleBassSwap,
	}
}
