// Package automix 实现歌曲间的智能过渡（混音过渡）。
//
// 移植自 folia-major 的 src/services/automix（AGPL-3.0，作者 chthollyphile 等）。
// 本包只含纯计算：离线测量（trackProfile）、乐理决策（planner / chooser）、
// 以及增益曲线、三频段、回声、软限幅的采样级实现。不依赖播放器或音频设备。
//
// 各常量与算法的取舍原因，以上游 src/services/automix/README.md 为准；
// 此处的注释只保留理解代码所必需的部分。
package automix

import "math"

const (
	minBPM = 60
	maxBPM = 180
	// MinTempoConfidence 以下的自相关峰与噪声无法区分。
	MinTempoConfidence = 0.35
	// SilenceDB 所有电平读数的下限。
	SilenceDB = -90.0

	loudOutroDB    = -14.0
	quietOutroDB   = -30.0
	silentTailDB   = -45.0
	CrossoverEarly = 0.35
	CrossoverLate  = 0.55
	// MaxTrimDB 出场曲为平衡响度最多被压低多少。
	MaxTrimDB = 6.0

	beatSnapTolerance  = 0.25
	downbeatWindowBars = 24
	tempoHarmonics     = 4
	phaseBeats         = 96
)

func clamp(v, lo, hi float64) float64 { return math.Min(hi, math.Max(lo, v)) }

// DbToGain dB 转线性增益。
func DbToGain(db float64) float64 { return math.Pow(10, db/20) }

// RmsDB 一帧的 RMS，单位 dBFS。
func RmsDB(samples []float32) float64 {
	if len(samples) == 0 {
		return SilenceDB
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	rms := math.Sqrt(sum / float64(len(samples)))
	if rms > 0 {
		return math.Max(SilenceDB, 20*math.Log10(rms))
	}
	return SilenceDB
}

// FFT 原地基 2 迭代 FFT。
func FFT(real, imag []float64) {
	n := len(real)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			real[i], real[j] = real[j], real[i]
			imag[i], imag[j] = imag[j], imag[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		angle := -2 * math.Pi / float64(size)
		wr, wi := math.Cos(angle), math.Sin(angle)
		half := size / 2
		for i := 0; i < n; i += size {
			cr, ci := 1.0, 0.0
			for k := 0; k < half; k++ {
				ar, ai := real[i+k], imag[i+k]
				br := real[i+k+half]*cr - imag[i+k+half]*ci
				bi := real[i+k+half]*ci + imag[i+k+half]*cr
				real[i+k], imag[i+k] = ar+br, ai+bi
				real[i+k+half], imag[i+k+half] = ar-br, ai-bi
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}

// SpectralFlux 只计能量上升：起音是新能量到来，衰减只是前一个声音在消失。
func SpectralFlux(current, previous []float32, bins int) float64 {
	limit := min(bins, len(current), len(previous))
	var flux float64
	for i := 0; i < limit; i++ {
		now, before := float64(current[i]), float64(previous[i])
		if math.IsInf(now, 0) || math.IsNaN(now) {
			now = SilenceDB
		}
		if math.IsInf(before, 0) || math.IsNaN(before) {
			before = SilenceDB
		}
		if now > before {
			flux += now - before
		}
	}
	return flux
}

// TempoEstimate 自相关测速结果。
type TempoEstimate struct {
	BPM        float64
	PeriodSec  float64
	Confidence float64
	// BeatOffsetHops 包络最新样本之前多少个 hop 处落着最近一拍。
	BeatOffsetHops int
}

func tempoPrior(bpm float64) float64 {
	x := math.Log2(bpm/120) / 0.55
	return math.Exp(-0.5 * x * x)
}

// EstimateTempo 由起音强度包络估计速度与拍相位。无周期性时返回 nil——错的网格比没有网格更糟。
func EstimateTempo(envelope []float64, hopSec float64) *TempoEstimate {
	if !(hopSec > 0) {
		return nil
	}
	count := len(envelope)
	minLag := max(2, int(math.Round(60/(maxBPM*hopSec))))
	maxLag := int(math.Round(60 / (minBPM * hopSec)))
	if count < maxLag*3 {
		return nil
	}
	var mean float64
	for _, v := range envelope {
		mean += v
	}
	mean /= float64(count)

	centred := make([]float64, count)
	var energy float64
	for i, v := range envelope {
		c := math.Max(0, v-mean)
		centred[i] = c
		energy += c * c
	}
	if energy <= 0 {
		return nil
	}

	topLag := min(count/3, maxLag*tempoHarmonics)
	correlation := make([]float64, topLag+1)
	for lag := minLag; lag <= topLag; lag++ {
		var sum float64
		for i := lag; i < count; i++ {
			sum += centred[i] * centred[i-lag]
		}
		correlation[lag] = sum / float64(count-lag)
	}

	bestLag, bestScore, scoreSum, scored := 0, 0.0, 0.0, 0
	for lag := minLag; lag <= maxLag; lag++ {
		var sum float64
		for h := 1; h <= tempoHarmonics; h++ {
			at := lag * h
			if at > topLag {
				break
			}
			sum += correlation[at] / float64(h)
		}
		score := sum * tempoPrior(60/(float64(lag)*hopSec))
		scoreSum += score
		scored++
		if score > bestScore {
			bestScore, bestLag = score, lag
		}
	}
	average := 0.0
	if scored > 0 {
		average = scoreSum / float64(scored)
	}
	if bestLag == 0 || bestScore <= 0 || average <= 0 {
		return nil
	}

	// 周期精确到 hop 以下：抛物线顶点 + 在第 h 个倍数处读数再除以 h。
	refine := func(lag int) float64 {
		for h := tempoHarmonics; h >= 1; h-- {
			centre := lag * h
			if centre+1 > topLag {
				continue
			}
			peak := centre
			from := max(minLag+1, centre-h)
			to := min(topLag-1, centre+h)
			for at := from; at <= to; at++ {
				if correlation[at] > correlation[peak] {
					peak = at
				}
			}
			if peak <= minLag || peak >= topLag {
				continue
			}
			before, after := correlation[peak-1], correlation[peak+1]
			curve := before - 2*correlation[peak] + after
			if !(curve < 0) {
				continue
			}
			period := (float64(peak) + (before-after)/(2*curve)) / float64(h)
			if math.Abs(period-float64(lag)) <= 0.5 {
				return period
			}
		}
		return float64(lag)
	}
	period := refine(bestLag)

	beatOffsetHops, bestPhase := 0, -1.0
	for offset := 0; offset < bestLag; offset++ {
		var sum float64
		for beat := 0; beat < phaseBeats; beat++ {
			idx := int(math.Round(float64(count-1-offset) - float64(beat)*period))
			if idx < 0 {
				break
			}
			sum += centred[idx]
		}
		if sum > bestPhase {
			bestPhase, beatOffsetHops = sum, offset
		}
	}
	return &TempoEstimate{
		BPM:            60 / (period * hopSec),
		PeriodSec:      period * hopSec,
		Confidence:     clamp((bestScore/average-1)/2, 0, 1),
		BeatOffsetHops: beatOffsetHops,
	}
}

// CrossoverFor 过渡中两曲在何处换位：响而密的结尾早让位，安静衰减的尾巴允许多跑一会儿。
func CrossoverFor(outgoingDB *float64) float64 {
	if outgoingDB == nil || math.IsInf(*outgoingDB, 0) || math.IsNaN(*outgoingDB) {
		return (CrossoverEarly + CrossoverLate) / 2
	}
	if *outgoingDB <= silentTailDB {
		return CrossoverEarly
	}
	loudness := clamp((*outgoingDB-quietOutroDB)/(loudOutroDB-quietOutroDB), 0, 1)
	return CrossoverLate + (CrossoverEarly-CrossoverLate)*loudness
}

// TrimForBalance 只衰减、只衰减出场曲。
func TrimForBalance(outgoingDB, incomingDB float64) float64 {
	return clamp(outgoingDB-incomingDB, 0, MaxTrimDB)
}

// BlendShapeInput planBlendShape 的输入。
type BlendShapeInput struct {
	Overlap    float64
	OutgoingDB *float64
	NextBeatIn *float64
	PeriodSec  *float64
	MinOverlap float64
	MaxOverlap float64
}

// BlendShape 实际排程的过渡形状。
type BlendShape struct {
	Overlap       float64
	Crossover     float64
	SnappedToBeat bool
}

// PlanBlendShape 把换位点对齐到出场曲的拍上（移动的是长度，不是起点）。
func PlanBlendShape(in BlendShapeInput) BlendShape {
	crossover := CrossoverFor(in.OutgoingDB)
	shape := BlendShape{Overlap: in.Overlap, Crossover: crossover}
	if in.NextBeatIn == nil || in.PeriodSec == nil || !(*in.PeriodSec > 0) {
		return shape
	}
	next, period := *in.NextBeatIn, *in.PeriodSec
	low := math.Max(in.MinOverlap, in.Overlap*(1-beatSnapTolerance))
	high := math.Min(in.MaxOverlap, in.Overlap*(1+beatSnapTolerance))
	if low > high {
		return shape
	}
	target := in.Overlap * crossover
	beat := next + math.Round((target-next)/period)*period
	if !(beat > 0) {
		return shape
	}
	overlap := beat / crossover
	if overlap < low || overlap > high {
		return shape
	}
	return BlendShape{Overlap: overlap, Crossover: crossover, SnappedToBeat: true}
}

// BlendHeadroomDB 两份母带叠加期间留出的余量，两端为 0、中间满额。
const BlendHeadroomDB = 1.5

func headroomBell(progress float64) float64 { return math.Sin(clamp(progress, 0, 1) * math.Pi) }

// CrossfadeGains 过渡中 progress（0..1）处两路的增益。
//
// 上游用 64 点曲线交给 setValueCurveAtTime 线性插值；这里直接按采样求值，形状一致。
// together 是中段"平台"占比：淡入淡出与混音的区别是形状而不是长度。
func CrossfadeGains(progress, crossover, together float64) (out, in float64) {
	pivot := clamp(crossover, 0.15, 0.85)
	held := clamp(together, 0, 0.8)
	lead := pivot * (1 - held)
	trail := pivot + (1-pivot)*held
	p := clamp(progress, 0, 1)
	var warped float64
	switch {
	case p <= lead:
		warped = p / math.Max(1e-3, lead) * 0.5
	case p >= trail:
		warped = 0.5 + (p-trail)/math.Max(1e-3, 1-trail)*0.5
	default:
		warped = 0.5
	}
	angle := warped * math.Pi / 2
	headroom := DbToGain(-BlendHeadroomDB * headroomBell(p))
	return math.Cos(angle) * headroom, math.Sin(angle) * headroom
}

// ToneEdgeHz 三个频段的分界。
var ToneEdgeHz = [2]float64{250, 4000}

// ToneMidHz 中频段几何中心，峰值滤波器的频率。
var ToneMidHz = math.Round(math.Sqrt(ToneEdgeHz[0] * ToneEdgeHz[1]))

const (
	bassKillDB     = 24.0
	sweepOutHighDB = 9.0
	bassSwapWidth  = 0.12
	MaxTiltDB      = 3.0
)

// BandBlendRequest 三频段接缝的参数。
type BandBlendRequest struct {
	Crossover float64
	SwapBass  bool
	SweepOut  bool
	TiltDB    [2]float64
}

// BandGains 过渡中 progress 处两路 [low, mid, high] 三个滤波器的增益（dB）。
//
// 低频在换位点一刀交接；中频随总曲线；出场曲高频在换位后加速离开；
// 进场曲中高频先带着出场曲的音色进来，在换位点前回到自己。
func BandGains(progress float64, r BandBlendRequest) (out, in [3]float64) {
	seam := clamp(r.Crossover, 0.15, 0.85)
	width := math.Max(0.02, math.Min(bassSwapWidth, math.Min(seam, 1-seam)))
	tiltMid := clamp(r.TiltDB[0], -MaxTiltDB, MaxTiltDB)
	tiltHigh := clamp(r.TiltDB[1], -MaxTiltDB, MaxTiltDB)
	p := clamp(progress, 0, 1)
	handover := clamp((p-(seam-width/2))/width, 0, 1)
	leaving := clamp((p-seam)/math.Max(1e-3, 1-seam), 0, 1)
	leaving *= leaving
	settling := clamp(1-p/math.Max(1e-3, seam), 0, 1)
	if r.SwapBass {
		out[0] = -bassKillDB * handover
		in[0] = -bassKillDB * (1 - handover)
	}
	if r.SweepOut {
		out[2] = -sweepOutHighDB * leaving
	}
	in[1] = tiltMid * settling
	in[2] = tiltHigh * settling
	return
}

// ToneTilt 进场曲中/高频应先弯多少 dB，才能以出场曲的音色到达。
func ToneTilt(outgoing, incoming []float64) [2]float64 {
	if len(outgoing) < 3 || len(incoming) < 3 {
		return [2]float64{}
	}
	bend := func(band int) float64 {
		a, b := outgoing[band], incoming[band]
		if !(a > 0) || !(b > 0) {
			return 0
		}
		return clamp(10*math.Log10(a/b), -MaxTiltDB, MaxTiltDB)
	}
	return [2]float64{bend(1), bend(2)}
}

// LufsOffsetDB K 计权均方功率转 LUFS 的偏移（ITU-R BS.1770）。
const LufsOffsetDB = -0.691

// KWeight 两级双二阶 K 计权滤波，系数按实际采样率由模拟原型推导。
func KWeight(samples []float32, sampleRate float64) []float32 {
	out := make([]float32, len(samples))
	if !(sampleRate > 0) || len(samples) == 0 {
		return out
	}
	const (
		shelfHz     = 1681.974450955533
		shelfGainDB = 3.999843853973347
		shelfQ      = 0.7071752369554196
		hpHz        = 38.13547087602444
		hpQ         = 0.5003270373238773
	)
	k1 := math.Tan(math.Pi * shelfHz / sampleRate)
	vh := math.Pow(10, shelfGainDB/20)
	vb := math.Pow(vh, 0.4996667741545416)
	d0 := 1 + k1/shelfQ + k1*k1
	b0 := (vh + vb*k1/shelfQ + k1*k1) / d0
	b1 := 2 * (k1*k1 - vh) / d0
	b2 := (vh - vb*k1/shelfQ + k1*k1) / d0
	a1 := 2 * (k1*k1 - 1) / d0
	a2 := (1 - k1/shelfQ + k1*k1) / d0

	k2 := math.Tan(math.Pi * hpHz / sampleRate)
	d1 := 1 + k2/hpQ + k2*k2
	c1 := 2 * (k2*k2 - 1) / d1
	c2 := (1 - k2/hpQ + k2*k2) / d1

	var x1, x2, y1, y2, p1, p2, q1, q2 float64
	for i, s := range samples {
		x := float64(s)
		y := b0*x + b1*x1 + b2*x2 - a1*y1 - a2*y2
		x2, x1, y2, y1 = x1, x, y1, y
		z := y - 2*p1 + p2 - c1*q1 - c2*q2
		p2, p1, q2, q1 = p1, y, q1, z
		out[i] = float32(z)
	}
	return out
}

// BeatsPerBar 一小节几拍。
const BeatsPerBar = 4

type phaseVote struct {
	phase    int
	contrast float64
}

func votePhase(envelope []float64, hopSec, periodSec, beatOffsetSec float64, beatsPerBar int) *phaseVote {
	radius := max(1, int(math.Round(periodSec/hopSec/8)))
	at := func(seconds float64) (float64, bool) {
		centre := int(math.Round(seconds / hopSec))
		if centre < 0 || centre >= len(envelope) {
			return 0, false
		}
		peak := envelope[centre]
		for i := centre - radius; i <= centre+radius; i++ {
			if i >= 0 && i < len(envelope) && envelope[i] > peak {
				peak = envelope[i]
			}
		}
		return peak, true
	}
	scores := make([]float64, beatsPerBar)
	span := float64(len(envelope)) * hopSec
	barSec := periodSec * float64(beatsPerBar)
	for phase := 0; phase < beatsPerBar; phase++ {
		var hits []float64
		first := beatOffsetSec + float64(phase)*periodSec
		bars := int(math.Floor((span-first)/barSec)) + 1
		for bar := max(0, bars-downbeatWindowBars); bar < bars; bar++ {
			if v, ok := at(first + float64(bar)*barSec); ok {
				hits = append(hits, v)
			}
		}
		scores[phase] = median(hits)
	}
	best := 0
	for phase := 1; phase < beatsPerBar; phase++ {
		if scores[phase] > scores[best] {
			best = phase
		}
	}
	var mean float64
	for _, s := range scores {
		mean += s
	}
	mean /= float64(len(scores))

	windowStart := min(len(envelope)-1, max(0, int(math.Round((span-downbeatWindowBars*barSec)/hopSec))))
	var floor float64
	for i := windowStart; i < len(envelope); i++ {
		floor += envelope[i]
	}
	floor /= float64(len(envelope) - windowStart)
	if !(mean > 0) || scores[best] < mean*1.25 || scores[best] < floor*1.5 {
		return nil
	}
	return &phaseVote{phase: best, contrast: scores[best] / mean}
}

// median 与上游一致：排序后取 len>>1。
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sortFloats(sorted)
	return sorted[len(sorted)>>1]
}

// EstimateDownbeat 哪一拍是"一"：底鼓与和声变化两种证据投票，取对比度高者。
// 返回自包络起点到某个重拍的秒数；无法区分时返回 nil。
func EstimateDownbeat(lowEnvelope []float64, hopSec, periodSec, beatOffsetSec float64, beatsPerBar int, harmonicEnvelope []float64) *float64 {
	if !(hopSec > 0) || !(periodSec > 0) || len(lowEnvelope) < beatsPerBar*4 {
		return nil
	}
	kick := votePhase(lowEnvelope, hopSec, periodSec, beatOffsetSec, beatsPerBar)
	var harmony *phaseVote
	if len(harmonicEnvelope) >= beatsPerBar*4 {
		harmony = votePhase(harmonicEnvelope, hopSec, periodSec, beatOffsetSec, beatsPerBar)
	}
	winner := kick
	switch {
	case kick == nil:
		winner = harmony
	case harmony != nil && harmony.contrast > kick.contrast:
		winner = harmony
	}
	if winner == nil {
		return nil
	}
	barSec := periodSec * float64(beatsPerBar)
	v := modulo(beatOffsetSec+float64(winner.phase)*periodSec, barSec)
	return &v
}

func modulo(value, span float64) float64 {
	return math.Mod(math.Mod(value, span)+span, span)
}
