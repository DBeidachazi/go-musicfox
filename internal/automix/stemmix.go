package automix

import (
	"math"
)

// StemWindowSec 分离窗口长度。由规划器可要求的最长重叠（25s）锁定，
// 不可为省内存缩短：盖不住重叠的窗口会让整个手法退回主交叉淡化。
const StemWindowSec = 30.0

// StemSampleRate htdemucs 的采样率。
const StemSampleRate = 44100

// StemRole 窗口取自曲尾（出场）还是曲首（进场）。
type StemRole string

const (
	RoleHead StemRole = "head"
	RoleTail StemRole = "tail"
)

// StemWindow 一段分离结果：四条立体声音轨，与原混音逐采样对齐。
//
// other 由相减得出，因此四轨相加精确等于原曲：单位增益下"整轨 → 四轨"的切换是真正的 no-op。
// 按 Int16 + 每轨峰值除数存储（每窗口约 21MB 而非 42MB），峰值除数保证 other 这种会过冲的轨不被削平。
type StemWindow struct {
	Rate float64
	// From 窗口首个采样在曲中的秒数。
	From float64
	Role StemRole
	// VocalEnd 窗口里最后还在唱的时刻（曲中秒数），仅曲尾窗口，没唱为 nil。
	VocalEnd *float64
	pcm      [StemCount][2][]int16
	gain     [StemCount]float64
	length   int
	// vocalEnv / mixEnv 整个窗口按 StemCellSec 的人声与四轨和包络，在后台预先算好，
	// 使过渡开始时（音频回调里）只需切片。
	vocalEnv, mixEnv []float32
}

// NewStemWindow 由四条浮点音轨（顺序 drums, bass, other, vocals）构造。
func NewStemWindow(rate, from float64, role StemRole, stems [StemCount][2][]float32) *StemWindow {
	w := &StemWindow{Rate: rate, From: from, Role: role, length: len(stems[0][0])}
	for s := 0; s < StemCount; s++ {
		peak := 0.0
		for ch := 0; ch < 2; ch++ {
			for _, v := range stems[s][ch] {
				peak = math.Max(peak, math.Abs(float64(v)))
			}
		}
		w.gain[s] = math.Max(1, peak)
		scale := 32767 / w.gain[s]
		for ch := 0; ch < 2; ch++ {
			src := stems[s][ch]
			dst := make([]int16, w.length)
			for i := 0; i < w.length && i < len(src); i++ {
				dst[i] = int16(math.Round(float64(src[i]) * scale))
			}
			w.pcm[s][ch] = dst
		}
	}
	return w
}

// PrepareEnvelopes 计算整窗包络。分离完成后在后台调用一次。
func (w *StemWindow) PrepareEnvelopes() {
	if w.vocalEnv != nil {
		return
	}
	w.vocalEnv = EnvelopeOf(w.Channels(StemVocals, 0, w.length), w.Rate, StemCellSec)
	w.mixEnv = EnvelopeOf(w.MixChannels(0, w.length), w.Rate, StemCellSec)
}

// envelopes 从第 at 个采样起 seconds 秒的人声与混音包络（按单元对齐，误差不超过半个单元）。
func (w *StemWindow) envelopes(at int, seconds float64) (vocals, mix []float32) {
	w.PrepareEnvelopes()
	first := int(math.Round(float64(at) / (StemCellSec * w.Rate)))
	cells := int(math.Floor(seconds/StemCellSec + 1e-9))
	first = min(max(0, first), len(w.vocalEnv))
	last := min(first+cells, len(w.vocalEnv))
	return w.vocalEnv[first:last], w.mixEnv[first:last]
}

// Len 采样数。
func (w *StemWindow) Len() int { return w.length }

// Duration 秒。
func (w *StemWindow) Duration() float64 { return float64(w.length) / w.Rate }

// Sample 第 s 条音轨第 i 个采样；越界为 0。
func (w *StemWindow) Sample(s, i int) [2]float64 {
	if i < 0 || i >= w.length {
		return [2]float64{}
	}
	k := w.gain[s] / 32767
	return [2]float64{float64(w.pcm[s][0][i]) * k, float64(w.pcm[s][1][i]) * k}
}

// Channels 某条音轨的浮点副本（用于测包络）。
func (w *StemWindow) Channels(s, from, n int) [][]float32 {
	out := [][]float32{make([]float32, n), make([]float32, n)}
	for i := 0; i < n; i++ {
		v := w.Sample(s, from+i)
		out[0][i], out[1][i] = float32(v[0]), float32(v[1])
	}
	return out
}

// MixChannels 四轨之和的浮点副本。
func (w *StemWindow) MixChannels(from, n int) [][]float32 {
	out := [][]float32{make([]float32, n), make([]float32, n)}
	for i := 0; i < n; i++ {
		for s := 0; s < StemCount; s++ {
			v := w.Sample(s, from+i)
			out[0][i] += float32(v[0])
			out[1][i] += float32(v[1])
		}
	}
	return out
}

// Resampled 换到另一个采样率（线性插值，窗口只在过渡期间播放、四轨同源，足够）。
func (w *StemWindow) Resampled(rate float64) *StemWindow {
	if rate == w.Rate || rate <= 0 {
		return w
	}
	n := int(float64(w.length) * rate / w.Rate)
	w.PrepareEnvelopes()
	out := &StemWindow{Rate: rate, From: w.From, Role: w.Role, VocalEnd: w.VocalEnd, gain: w.gain, length: n,
		vocalEnv: w.vocalEnv, mixEnv: w.mixEnv}
	ratio := w.Rate / rate
	for s := 0; s < StemCount; s++ {
		for ch := 0; ch < 2; ch++ {
			src := w.pcm[s][ch]
			dst := make([]int16, n)
			for i := range dst {
				x := float64(i) * ratio
				j := int(x)
				if j+1 >= len(src) {
					if j < len(src) {
						dst[i] = src[j]
					}
					continue
				}
				f := x - float64(j)
				dst[i] = int16(math.Round(float64(src[j])*(1-f) + float64(src[j+1])*f))
			}
			out.pcm[s][ch] = dst
		}
	}
	return out
}

// StemCovers 窗口覆盖曲中 [at, at+seconds)。
func (w *StemWindow) StemCovers(at, seconds float64) bool {
	return w != nil && at >= w.From-1e-6 && at+seconds <= w.From+w.Duration()+1e-6
}

// StemSpliceSec 整轨与四轨之间的拼接时长。
const StemSpliceSec = 0.008

// StemGesture 一次过渡的四轨交接，交给 Mixer 在重叠段逐采样执行。
type StemGesture struct {
	// Out / In 已换到输出采样率的窗口。
	Out, In *StemWindow
	// OutAt / InAt 重叠开始时两个窗口里的采样下标。
	OutAt, InAt int
	Handover    StemHandover
	// Wall 重叠秒数。
	Wall float64
	// Bars 出场曲窗口里的小节线（相对窗口起点），仅用于日志。
	Bars int
}

// StemGestureRequest 构造 StemGesture 所需的事实。
type StemGestureRequest struct {
	Out, In *StemWindow
	// OutStart / InStart 重叠开始时两首歌各自的曲中位置（秒）。
	OutStart, InStart float64
	Wall              float64
	Rate              float64
	From, To          *TrackProfile
	KeysClash         bool
}

func barSecOf(p *TrackProfile) *float64 {
	if p == nil || p.BPM == nil || *p.BPM <= 0 {
		return nil
	}
	per := p.BeatsPerBar
	if per <= 0 {
		per = BeatsPerBar
	}
	return ptr(60 / *p.BPM * float64(per))
}

// PlanStemGesture 窗口盖得住两端时给出交接；盖不住返回 nil 与原因。
func PlanStemGesture(r StemGestureRequest) (*StemGesture, string) {
	if r.Out == nil || r.In == nil {
		return nil, "not separated"
	}
	if !r.Out.StemCovers(r.OutStart, r.Wall) || !r.In.StemCovers(r.InStart, r.Wall) {
		return nil, "stems do not cover the window"
	}
	// 窗口若已预先换到输出采样率，这里就不再有任何按采样的工作。
	if r.Rate > 0 {
		if r.Out.Rate != r.Rate {
			r.Out = r.Out.Resampled(r.Rate)
		}
		if r.In.Rate != r.Rate {
			r.In = r.In.Resampled(r.Rate)
		}
	}
	rate := r.Out.Rate
	outAt := int(math.Round((r.OutStart - r.Out.From) * rate))
	inAt := int(math.Round((r.InStart - r.In.From) * r.In.Rate))
	n := int(math.Round(r.Wall * rate))
	nIn := int(math.Round(r.Wall * r.In.Rate))
	if outAt+n > r.Out.Len() || inAt+nIn > r.In.Len() {
		return nil, "stems do not cover the window"
	}
	// "安静"相对窗口里所有东西的和来衡量，同一个阈值才能同时用于抒情曲与吉他墙。
	vocals, mix := r.Out.envelopes(outAt, r.Wall)
	// 进场曲的参照是整段已分离的开头，而不是这个窗口。
	inVocals, _ := r.In.envelopes(inAt, r.Wall)
	r.In.PrepareEnvelopes()
	stemFindsVoice := SingsInWindow(inVocals, r.In.mixEnv)
	var bars []float64
	if r.From != nil {
		if g := BarGrid(r.From.BPM, r.From.DownbeatOffset, max(1, r.From.BeatsPerBar)); g != nil {
			first := math.Ceil((r.OutStart - g.Offset) / g.Period)
			for k := first; ; k++ {
				at := g.Offset + k*g.Period - r.OutStart
				if at > r.Wall {
					break
				}
				if at >= 0 {
					bars = append(bars, at)
				}
			}
		}
	}
	// 两个度量都同意才判定进场曲不唱：人声轨没找到，且第一个段落边界在窗口之后。
	var sings *bool
	if r.To != nil && r.To.SectionStart != nil && !stemFindsVoice && *r.To.SectionStart-r.InStart > r.Wall {
		sings = ptr(false)
	}
	h := PlanStemHandover(r.Wall, barSecOf(r.From), barSecOf(r.To), bars, vocals, mix,
		HandoverIncoming{KeysClash: r.KeysClash, Sings: sings}, StemCellSec)
	return &StemGesture{Out: r.Out, In: r.In, OutAt: outAt, InAt: inAt, Handover: h, Wall: r.Wall, Bars: len(bars)}, ""
}

// highpass 一阶以上的 RBJ 高通（与 Web Audio BiquadFilterNode 'highpass' 同定义）。
type highpass struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             [2]float64
	rate               float64
}

func (h *highpass) set(freq, q float64) {
	w0 := 2 * math.Pi * freq / h.rate
	cw, sw := math.Cos(w0), math.Sin(w0)
	alpha := sw / (2 * q)
	a0 := 1 + alpha
	h.b0 = (1 + cw) / 2 / a0
	h.b1 = -(1 + cw) / a0
	h.b2 = (1 + cw) / 2 / a0
	h.a1 = -2 * cw / a0
	h.a2 = (1 - alpha) / a0
}

func (h *highpass) process(ch int, x float64) float64 {
	y := h.b0*x + h.z1[ch]
	h.z1[ch] = h.b1*x - h.a1*y + h.z2[ch]
	h.z2[ch] = h.b2*x - h.a2*y
	return y
}

// stemPlayer 在 Mixer 里执行 StemGesture 的状态。
type stemPlayer struct {
	g      *StemGesture
	rate   float64
	n      int
	splice int
	sweep  *highpass
	outG   [StemCount]float64
	inG    [StemCount]float64
}

func newStemPlayer(g *StemGesture, rate float64) *stemPlayer {
	p := &stemPlayer{g: g, rate: rate, n: max(1, int(math.Round(g.Wall*rate))),
		splice: max(1, int(StemSpliceSec*rate)), sweep: &highpass{rate: rate}}
	p.sweep.set(OtherSweepHz[0], 0.7)
	return p
}

// mix 重叠段第 t 个采样：出场整轨 a（已乘 trim）、进场整轨 b，返回两者经四轨交接后的和。
//
// 出场：开头 8ms 从整轨拼到四轨，之后只剩四轨。进场：整窗走四轨，最后 8ms 拼回整轨，
// 因为窗口只盖过渡，歌还要继续。四轨在单位增益时之和等于原曲，所以拼接点是透明的。
func (p *stemPlayer) mix(t int, a, b [2]float64, trim float64) [2]float64 {
	g := p.g
	if t%coeffUpdateEvery == 0 {
		at := float64(t) / p.rate
		p.outG = g.Handover.OutgoingGains(g.Wall, at)
		p.inG = g.Handover.IncomingGains(g.Wall, at)
		// 出场 other 轨从下往上削薄：f0·(f1/f0)^t，与参考实现的指数扫频一致。
		frac := math.Min(1, at/(g.Wall*OtherSweepEnd))
		p.sweep.set(OtherSweepHz[0]*math.Pow(OtherSweepHz[1]/OtherSweepHz[0], frac), 0.7)
	}
	var outStems, inStems [2]float64
	for s := 0; s < StemCount; s++ {
		o := g.Out.Sample(s, g.OutAt+t)
		i := g.In.Sample(s, g.InAt+t)
		if s == StemOther {
			o[0], o[1] = p.sweep.process(0, o[0]), p.sweep.process(1, o[1])
		}
		for ch := 0; ch < 2; ch++ {
			outStems[ch] += o[ch] * p.outG[s]
			inStems[ch] += i[ch] * p.inG[s]
		}
	}
	elemOut, stemsIn, elemIn := 0.0, 1.0, 0.0
	if t < p.splice {
		elemOut = 1 - float64(t)/float64(p.splice)
	}
	if rest := p.n - t; rest <= p.splice {
		stemsIn = float64(rest) / float64(p.splice)
		elemIn = 1 - stemsIn
	}
	stemsOut := 1 - elemOut
	var mix [2]float64
	for ch := 0; ch < 2; ch++ {
		mix[ch] = a[ch]*elemOut + outStems[ch]*stemsOut*trim + inStems[ch]*stemsIn + b[ch]*elemIn
	}
	return mix
}
