package automix

import "math"

// 执行层：把 StyledBlend 变成逐采样的两路混合。
//
// 上游用 Web Audio：每路 deck 一条 trim → lowshelf → peaking → highshelf → fade 的节点链，
// 另有一条从 highshelf 之后取出的回声发送。这里在采样域做同样的事，
// 没有 AudioParam 调度，因此上游 README §9「执行」里那几条引擎规则在此不存在。

const (
	midBellQ         = 0.8
	defaultThrowSec  = 0.25
	throwFeedback    = 0.45
	throwSend        = 0.6
	throwOpenSec     = 0.15
	throwCloseSec    = 0.03
	throwRingSec     = 3.0
	balanceRampSec   = 0.4
	coeffUpdateEvery = 32
	limitKnee        = 0.95
)

// SoftLimit 0.95 以下是直线，之上单调、奇对称、无拐角。
func SoftLimit(x float64) float64 {
	level := math.Abs(x)
	if level <= limitKnee {
		return x
	}
	room := 1 - limitKnee
	held := limitKnee + room*math.Tanh((level-limitKnee)/room)
	if x < 0 {
		return -held
	}
	return held
}

// biquad 直接 II 型转置，立体声两路状态。
type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             [2]float64
	gainDB             float64
	kind               int
	freq, q            float64
	rate               float64
}

const (
	lowShelf = iota
	peaking
	highShelf
)

func newBiquad(kind int, freq, q, rate float64) *biquad {
	b := &biquad{kind: kind, freq: freq, q: q, rate: rate}
	b.set(0)
	return b
}

// set 按 RBJ Audio EQ Cookbook 计算系数，与 Web Audio BiquadFilterNode 的定义一致
// （shelf 的斜率 S = 1）。
func (b *biquad) set(gainDB float64) {
	b.gainDB = gainDB
	if gainDB == 0 {
		b.b0, b.b1, b.b2, b.a1, b.a2 = 1, 0, 0, 0, 0
		return
	}
	A := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * b.freq / b.rate
	cw, sw := math.Cos(w0), math.Sin(w0)
	var b0, b1, b2, a0, a1, a2 float64
	switch b.kind {
	case peaking:
		alpha := sw / (2 * b.q)
		b0, b1, b2 = 1+alpha*A, -2*cw, 1-alpha*A
		a0, a1, a2 = 1+alpha/A, -2*cw, 1-alpha/A
	case lowShelf, highShelf:
		alpha := sw / 2 * math.Sqrt(2) // S = 1
		sq := 2 * math.Sqrt(A) * alpha
		if b.kind == lowShelf {
			b0 = A * ((A + 1) - (A-1)*cw + sq)
			b1 = 2 * A * ((A - 1) - (A+1)*cw)
			b2 = A * ((A + 1) - (A-1)*cw - sq)
			a0 = (A + 1) + (A-1)*cw + sq
			a1 = -2 * ((A - 1) + (A+1)*cw)
			a2 = (A + 1) + (A-1)*cw - sq
		} else {
			b0 = A * ((A + 1) + (A-1)*cw + sq)
			b1 = -2 * A * ((A - 1) + (A+1)*cw)
			b2 = A * ((A + 1) + (A-1)*cw - sq)
			a0 = (A + 1) - (A-1)*cw + sq
			a1 = 2 * ((A - 1) - (A+1)*cw)
			a2 = (A + 1) - (A-1)*cw - sq
		}
	}
	b.b0, b.b1, b.b2, b.a1, b.a2 = b0/a0, b1/a0, b2/a0, a1/a0, a2/a0
}

func (b *biquad) process(ch int, x float64) float64 {
	y := b.b0*x + b.z1[ch]
	b.z1[ch] = b.b1*x - b.a1*y + b.z2[ch]
	b.z2[ch] = b.b2*x - b.a2*y
	return y
}

type toneStack struct{ bands [3]*biquad }

func newToneStack(rate float64) *toneStack {
	return &toneStack{bands: [3]*biquad{
		newBiquad(lowShelf, ToneEdgeHz[0], 0, rate),
		newBiquad(peaking, ToneMidHz, midBellQ, rate),
		newBiquad(highShelf, ToneEdgeHz[1], 0, rate),
	}}
}

func (t *toneStack) set(gains [3]float64) {
	for i, b := range t.bands {
		if b.gainDB != gains[i] {
			b.set(gains[i])
		}
	}
}

func (t *toneStack) process(s [2]float64) [2]float64 {
	for ch := 0; ch < 2; ch++ {
		x := s[ch]
		for _, b := range t.bands {
			x = b.process(ch, x)
		}
		s[ch] = x
	}
	return s
}

// MixerConfig 构造 Mixer 所需的全部参数，均已由规划与形状阶段算好。
type MixerConfig struct {
	SampleRate float64
	Blend      StyledBlend
	Plan       TransitionPlan
	// TrimDB 出场曲为平衡响度被压低的 dB（>= 0）。
	TrimDB float64
	// PeriodSec 出场曲拍长，决定回声的间隔。nil 用默认 0.25s。
	PeriodSec *float64
}

// Mixer 一次过渡的逐采样混合器。时间轴：[0, hold) 出场曲独奏、进场曲静音走过自己的前导静音；
// [hold, hold+overlap) 按曲线交接；之后只剩进场曲，外加回声尾巴。
type Mixer struct {
	cfg      MixerConfig
	pos      int
	hold     int
	overlap  int
	echoAt   int
	echoOn   bool
	bands    BandBlendRequest
	outTone  *toneStack
	inTone   *toneStack
	trim     float64
	delay    [][2]float64
	delayIdx int
	rampTrim int
	ringEnd  int
}

// NewMixer 构造混合器。
func NewMixer(cfg MixerConfig) *Mixer {
	rate := cfg.SampleRate
	m := &Mixer{
		cfg:      cfg,
		hold:     int(math.Round(cfg.Blend.Hold * rate)),
		overlap:  max(1, int(math.Round(cfg.Blend.Overlap*rate))),
		trim:     DbToGain(-cfg.TrimDB),
		rampTrim: int(balanceRampSec * rate),
	}
	if cfg.Blend.ShapeBands {
		m.bands = BandBlendRequest{
			Crossover: cfg.Blend.Crossover,
			SwapBass:  true,
			SweepOut:  cfg.Blend.SweepOut,
			TiltDB:    cfg.Plan.TiltDB,
		}
		m.outTone = newToneStack(rate)
		m.inTone = newToneStack(rate)
	}
	m.ringEnd = m.hold + m.overlap
	if cfg.Plan.EchoThrow {
		repeat := defaultThrowSec
		if cfg.PeriodSec != nil && *cfg.PeriodSec > 0 {
			repeat = math.Min(1, math.Max(0.08, *cfg.PeriodSec/2))
		}
		m.delay = make([][2]float64, max(1, int(repeat*rate)))
		m.echoOn = true
		// 切在自己 40ms 的开头；重叠则在两曲换位之处。
		at := cfg.Blend.Hold
		if cfg.Blend.Style != StyleBeatCut {
			at += cfg.Blend.Overlap * cfg.Blend.Crossover
		}
		m.echoAt = int(at * rate)
		m.ringEnd += int(throwRingSec * rate)
	}
	return m
}

// Done 过渡与回声尾巴都已结束，出场曲可以释放。
func (m *Mixer) Done() bool { return m.pos >= m.ringEnd }

// OutgoingDone 出场曲已经完全听不见（回声尾巴可能还在响）。
func (m *Mixer) OutgoingDone() bool { return m.pos >= m.hold+m.overlap }

func (m *Mixer) sendGain(pos int) float64 {
	rate := m.cfg.SampleRate
	open := m.echoAt - int(throwOpenSec*rate)
	closeEnd := m.echoAt + int(throwCloseSec*rate)
	switch {
	case pos < open || pos >= closeEnd:
		return 0
	case pos < m.echoAt:
		return throwSend * float64(pos-open) / float64(max(1, m.echoAt-open))
	default:
		return throwSend * (1 - float64(pos-m.echoAt)/float64(max(1, closeEnd-m.echoAt)))
	}
}

// Process 混合一段。outgoing / incoming 可以比 dst 短（流已结束），不足部分视为静音。
func (m *Mixer) Process(dst, outgoing, incoming [][2]float64) {
	for i := range dst {
		var a, b [2]float64
		if i < len(outgoing) {
			a = outgoing[i]
		}
		if i < len(incoming) {
			b = incoming[i]
		}
		t := m.pos - m.hold
		progress := 0.0
		if t >= 0 {
			progress = math.Min(1, float64(t)/float64(m.overlap))
		}
		gOut, gIn := 1.0, 0.0
		if t >= 0 {
			gOut, gIn = CrossfadeGains(progress, m.cfg.Blend.Crossover, m.cfg.Blend.Together)
			if t >= m.overlap {
				gOut, gIn = 0, 1
			}
		}

		trim := m.trim
		if m.rampTrim > 0 && m.pos < m.rampTrim {
			trim = 1 + (m.trim-1)*float64(m.pos)/float64(m.rampTrim)
		}
		a[0] *= trim
		a[1] *= trim

		if m.outTone != nil {
			if m.pos%coeffUpdateEvery == 0 {
				bp := 0.0
				if t >= 0 {
					bp = progress
				}
				outG, inG := BandGains(bp, m.bands)
				m.outTone.set(outG)
				m.inTone.set(inG)
			}
			if gOut > 0 || m.echoOn {
				a = m.outTone.process(a)
			}
			b = m.inTone.process(b)
		}

		var mix [2]float64
		for ch := 0; ch < 2; ch++ {
			mix[ch] = a[ch]*gOut + b[ch]*gIn
		}
		if m.echoOn {
			d := m.delay[m.delayIdx]
			send := m.sendGain(m.pos)
			for ch := 0; ch < 2; ch++ {
				mix[ch] += d[ch]
				m.delay[m.delayIdx][ch] = a[ch]*send + d[ch]*throwFeedback
			}
			m.delayIdx = (m.delayIdx + 1) % len(m.delay)
		}
		dst[i] = [2]float64{SoftLimit(mix[0]), SoftLimit(mix[1])}
		m.pos++
	}
}

// LevelMeter 出场曲的实时电平：逐帧 RMS 的衰减峰值（12 dB/s），休止不会被读成安静。
type LevelMeter struct {
	rate   float64
	frame  []float32
	peakDB float64
	frames int
}

const (
	meterFrame        = 1024
	peakDecayDBPerSec = 12.0
	minFramesForLevel = 16
)

// NewLevelMeter 构造电平表。
func NewLevelMeter(rate float64) *LevelMeter {
	return &LevelMeter{rate: rate, peakDB: SilenceDB, frame: make([]float32, 0, meterFrame)}
}

// Feed 送入一段立体声采样。
func (l *LevelMeter) Feed(samples [][2]float64) {
	for _, s := range samples {
		l.frame = append(l.frame, float32((s[0]+s[1])/2))
		if len(l.frame) == meterFrame {
			level := RmsDB(l.frame)
			decay := peakDecayDBPerSec * meterFrame / l.rate
			l.peakDB = math.Max(level, l.peakDB-decay)
			l.frames++
			l.frame = l.frame[:0]
		}
	}
}

// DB 最近电平；读数不足约 0.4 秒时为 nil。
func (l *LevelMeter) DB() *float64 {
	if l.frames < minFramesForLevel {
		return nil
	}
	return ptr(l.peakDB)
}

// Reset 换歌时清空。
func (l *LevelMeter) Reset() {
	l.peakDB, l.frames, l.frame = SilenceDB, 0, l.frame[:0]
}
