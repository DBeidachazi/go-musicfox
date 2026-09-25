package automix

import (
	"math"
	"sort"
)

// 第十一轮的交接，纯算术（上游 stemGesture.ts 的移植）。不涉及音频节点与时钟：
// 给定两个窗口的包络与小节长度，说出每条音轨何时易手、出场人声如何离开。
//
// 这是移植，不是设计。每个数字都来自十一轮盲听，其中几个反直觉到差点被反着上线；
// 理由记在上游每个常量旁边。要改其中任何一个，需要带"什么都不做"对照组的盲听，而不是论证。

// StemCellSec 包络的单元长度，50ms。
const StemCellSec = 0.05

const (
	// stemRestDB 出场人声低于窗口混音中位电平多少才算休止，可以切而不是淡。
	stemRestDB = -30.0
	// stemCutSec 切的时长。半秒像剪辑，更短像掉线。
	stemCutSec = 0.5
	// stemExitFloorSec 退场永远不从 0 开始：窗口的第一刻是拼接落点。
	stemExitFloorSec = 0.05
	// stemMinRecedeSec 淡出最短一秒，再短就不是淡出而是消失。
	stemMinRecedeSec = 1.0
	// stemDeepRestDB 安静到切掉也不带走任何东西，可以用最快的切。
	stemDeepRestDB = -45.0
	// stemSoftExitSec 休止勉强够安静时切最多拉长到的秒数。
	stemSoftExitSec = 1.2
	// stemSustainMinSec 保持多久才算长音而不是长音节。
	stemSustainMinSec = 1.2
	// stemSustainFlatDB 长音可以偏离自身峰值多少仍算同一个音。
	stemSustainFlatDB = 7.0
	// stemSustainCells 连续多少个单元高于阈值才算在唱（分离会把镲片漏进人声轨）。
	stemSustainCells = 5
	// stemVocalTailSec 最后一个发声单元之后再加多少才算唱完；所有误差都必须偏晚。
	stemVocalTailSec = 0.3
	// stemSwapTarget 鼓换手的目标位置（窗口比例），有小节线时吸附过去。
	stemSwapTarget = 0.42
	// stemMaxTailBars 低频换手之后最多还剩几小节混音。
	stemMaxTailBars = 2.0
	// stemTailGuardSec 最后一个动作之后留出的余量。
	stemTailGuardSec = 0.3
	// StemSwapEdgeSec 一条音轨易手的速度：6ms，是换而不是淡。
	StemSwapEdgeSec = 0.006
)

// OtherSweepHz 出场曲 other 轨高通扫频的起止频率。
var OtherSweepHz = [2]float64{25, 2200}

// OtherSweepEnd 扫频占窗口的比例，早于该轨自己的淡出结束。
const OtherSweepEnd = 0.92

// VocalExitKind 出场人声离开的方式。
type VocalExitKind string

const (
	ExitRest    VocalExitKind = "rest"
	ExitRecede  VocalExitKind = "recede"
	ExitRelease VocalExitKind = "release"
)

// VocalSustain 出场人声正保持的一个长音。
type VocalSustain struct {
	From, To float64
	// HoldDB 高于混音中位电平多少 dB。
	HoldDB float64
}

// VocalExit 出场人声的退场。
type VocalExit struct {
	From, To float64
	Kind     VocalExitKind
	// LoudDB 可达范围内最安静半秒的电平，相对窗口中位。
	LoudDB float64
	Held   *VocalSustain
}

// StemHandover 一个窗口里每条音轨的交接时刻（相对窗口起点，秒）。
type StemHandover struct {
	Swap    float64
	BassAt  float64
	VocalIn float64
	// DueAt 编排本身会把进场人声放在哪里；与 VocalIn 不同说明骑长音推迟了它。
	DueAt float64
	Exit  VocalExit
}

// EnvelopeOf 一条音轨逐单元 RMS，立体声先混成单声道。
func EnvelopeOf(channels [][]float32, sampleRate, cellSec float64) []float32 {
	if len(channels) == 0 {
		return nil
	}
	cell := max(1, int(jsRound(cellSec*sampleRate)))
	length := len(channels[0])
	cells := length / cell
	out := make([]float32, cells)
	for c := 0; c < cells; c++ {
		var s float64
		for i := c * cell; i < (c+1)*cell; i++ {
			var frame float64
			for _, ch := range channels {
				frame += float64(ch[i])
			}
			frame /= float64(len(channels))
			s += frame * frame
		}
		out[c] = float32(math.Sqrt(s / float64(cell)))
	}
	return out
}

func medianF32(values []float32) float64 {
	if len(values) == 0 {
		return 0
	}
	s := make([]float64, len(values))
	for i, v := range values {
		s[i] = float64(v)
	}
	sort.Float64s(s)
	m := len(s) >> 1
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func dbOf(ratio float64) float64 { return 20 * math.Log10(math.Max(ratio, 1e-12)) }

// FindSustain 出场人声在窗口里保持的最后一个长音，没有则为 nil。
func FindSustain(vocals, mix []float32, cellSec float64) *VocalSustain {
	reference := math.Max(medianF32(mix), 1e-12)
	floor := reference * math.Pow(10, stemRestDB/20)
	band := math.Pow(10, -stemSustainFlatDB/20)
	need := max(1, int(jsRound(stemSustainMinSec/cellSec)))
	var held *VocalSustain
	start := -1
	peak := 0.0
	// 多走一格，窗口结束时仍在响的音由循环收尾而不是被丢掉。
	for cell := 0; cell <= len(vocals); cell++ {
		level := 0.0
		if cell < len(vocals) {
			level = float64(vocals[cell])
		}
		if start >= 0 && level > floor && level >= peak*band {
			if level > peak {
				peak = level
			}
			continue
		}
		if start >= 0 && cell-start >= need {
			held = &VocalSustain{From: float64(start) * cellSec, To: float64(cell) * cellSec, HoldDB: dbOf(peak / reference)}
		}
		if level > floor {
			start = cell
		} else {
			start = -1
		}
		peak = level
	}
	return held
}

// PlanVocalExit 出场人声如何离开：休止里切，否则淡出；保持中的长音骑到它自己的释放。
func PlanVocalExit(vocals, mix []float32, hardEnd, recedeFrom, cellSec float64, mayRide bool) VocalExit {
	reference := medianF32(mix)
	span := max(1, int(jsRound(stemCutSec/cellSec)))
	// 半秒里人声最响的时刻，而不是平均：平均会把停顿里的一个音节藏起来。
	loudAt := func(cell int) float64 {
		peak := 0.0
		for c := cell; c < cell+span; c++ {
			if c >= 0 && c < len(vocals) {
				peak = math.Max(peak, float64(vocals[c]))
			}
		}
		return dbOf(peak / math.Max(reference, 1e-12))
	}
	type pick struct {
		cell  int
		value float64
	}
	firstCell := int(jsRound(recedeFrom / cellSec))
	lastCell := int(math.Floor((hardEnd - stemCutSec) / cellSec))
	last := pick{-1, math.Inf(1)}
	quietest := pick{firstCell, math.Inf(1)}
	// 从 recedeFrom 起找，并且在够安静的时刻里取最晚的，而不是最安静的：
	// 取早的会删掉中间唱的每一个字（"吞了一句歌词"）。
	for cell := firstCell; cell <= lastCell; cell++ {
		v := loudAt(cell)
		if v < quietest.value {
			quietest = pick{cell, v}
		}
		if v <= stemRestDB {
			last = pick{cell, v}
		}
	}
	if math.IsInf(quietest.value, 0) {
		quietest = pick{firstCell, loudAt(firstCell)}
	}
	best := quietest
	if last.cell >= 0 {
		best = last
	}
	at := float64(best.cell) * cellSec
	// 切的速度按落点有多安静分级：深静音用半秒，勉强的休止最多一秒出头。
	softness := math.Min(1, math.Max(0, (best.value-stemDeepRestDB)/(stemRestDB-stemDeepRestDB)))
	exitSec := stemCutSec + softness*(stemSoftExitSec-stemCutSec)
	held := FindSustain(vocals, mix, cellSec)

	windowSec := float64(len(vocals)) * cellSec
	ridable := mayRide && held != nil && held.From <= hardEnd && held.To > hardEnd && held.To <= windowSec-cellSec
	if ridable {
		return VocalExit{From: held.To, To: math.Min(held.To+stemCutSec, windowSec), Kind: ExitRelease, LoudDB: best.value, Held: held}
	}
	if last.cell >= 0 {
		return VocalExit{From: at, To: math.Min(at+exitSec, hardEnd), Kind: ExitRest, LoudDB: best.value, Held: held}
	}
	// 无处可藏就淡出：耳朵原谅决定，不原谅漂移。
	return VocalExit{From: recedeFrom, To: hardEnd, Kind: ExitRecede, LoudDB: best.value, Held: held}
}

// LastVocalMoment 窗口里出场曲最后还在唱的时刻（相对窗口起点）；整窗没唱为 nil。
func LastVocalMoment(vocals, mix []float32, cellSec float64) *float64 {
	floor := medianF32(mix) * math.Pow(10, stemRestDB/20)
	run, edge := 0, -1
	for cell := len(vocals) - 1; cell >= 0; cell-- {
		if float64(vocals[cell]) <= floor {
			run = 0
			continue
		}
		if run == 0 {
			edge = cell
		}
		run++
		if run >= stemSustainCells {
			return ptr(float64(edge+1)*cellSec + stemVocalTailSec)
		}
	}
	return nil
}

// SingsInWindow 进场曲在窗口里是否唱了。只有否定结论可用（分离的串音在多数开头都能过阈值）。
// reference 是整个已分离的开头，而不是这个窗口——这正是它能用与不能用的全部区别。
func SingsInWindow(vocals, reference []float32) bool {
	floor := medianF32(reference) * math.Pow(10, stemRestDB/20)
	run := 0
	for _, v := range vocals {
		if float64(v) <= floor {
			run = 0
			continue
		}
		run++
		if run >= stemSustainCells {
			return true
		}
	}
	return false
}

// HandoverIncoming 进场曲自己的分离结果说了什么。
type HandoverIncoming struct {
	// KeysClash 两个调已知相冲（半音或三全音），不许骑长音。
	KeysClash bool
	// Sings nil 表示假定会唱；false 取消出场人声的截止期。
	Sings *bool
}

// PlanStemHandover 一个窗口里每条音轨何时易手：鼓先且几乎瞬间，贝斯晚一小节，
// 进场人声在鼓之后一个（进场曲的）小节。鼓与贝斯分开换，低频在律动里交接。
func PlanStemHandover(windowSec float64, outgoingBarSec, incomingBarSec *float64, downbeats []float64,
	vocals, mix []float32, incoming HandoverIncoming, cellSec float64,
) StemHandover {
	bar := windowSec / 4
	if outgoingBarSec != nil && *outgoingBarSec > 0 {
		bar = *outgoingBarSec
	}
	target := math.Max(stemSwapTarget*windowSec, windowSec-(1+stemMaxTailBars)*bar)
	swap := target
	first := true
	for _, at := range downbeats {
		if at < 1 || at > windowSec-1.5 {
			continue
		}
		if first || math.Abs(at-target) < math.Abs(swap-target) {
			swap = at
			first = false
		}
	}
	bassAt := math.Min(swap+bar, windowSec-stemTailGuardSec)
	inBar := bar
	if incomingBarSec != nil && *incomingBarSec > 0 {
		inBar = *incomingBarSec
	}
	noOneWaiting := incoming.Sings != nil && !*incoming.Sings
	entry := math.Min(windowSec-0.6, swap+inBar)
	hardEnd := math.Min(entry+stemCutSec, windowSec-0.4)
	recedeFloor, holdUntil := stemMinRecedeSec, swap
	if noOneWaiting {
		entry, hardEnd = windowSec, windowSec
		recedeFloor = stemCutSec
		holdUntil = hardEnd - recedeFloor
	}
	recedeFrom := math.Max(stemExitFloorSec, math.Min(holdUntil, hardEnd-recedeFloor))
	exit := PlanVocalExit(vocals, mix, hardEnd, recedeFrom, cellSec, !incoming.KeysClash)
	// 出场人声骑长音时，进场人声等这个音：只会推迟，不会提前。
	vocalIn := entry
	if exit.Kind == ExitRelease {
		vocalIn = math.Max(entry, math.Min(exit.From-inBar, windowSec-0.6))
	}
	return StemHandover{Swap: swap, BassAt: bassAt, VocalIn: vocalIn, DueAt: entry, Exit: exit}
}

// Rise 等功率从 0 升到 1。
func Rise(from, to, at float64) float64 {
	if at <= from {
		return 0
	}
	if at >= to {
		return 1
	}
	return math.Sin((at - from) / (to - from) * math.Pi / 2)
}

// Fall 刻意不是 Rise 的等功率补：cos(rise·π/2)，与每轮盲听所用的工具一致（中点下凹 1.6dB）。
func Fall(from, to, at float64) float64 { return math.Cos(Rise(from, to, at) * math.Pi / 2) }

// 四条音轨在数组中的顺序。
const (
	StemDrums = iota
	StemBass
	StemOther
	StemVocals
	StemCount
)

// OutgoingGains 出场曲四条音轨在窗口内 at 秒时的增益。
func (h StemHandover) OutgoingGains(windowSec, at float64) [StemCount]float64 {
	return [StemCount]float64{
		StemDrums:  Fall(h.Swap, h.Swap+StemSwapEdgeSec, at),
		StemBass:   Fall(h.BassAt, h.BassAt+StemSwapEdgeSec, at),
		StemOther:  Fall(windowSec*0.92, windowSec, at),
		StemVocals: Fall(h.Exit.From, h.Exit.To, at),
	}
}

// IncomingGains 进场曲的增益。刻意不是镜像：other 在自己的鼓之前就进来，先作为氛围存在。
func (h StemHandover) IncomingGains(windowSec, at float64) [StemCount]float64 {
	return [StemCount]float64{
		StemDrums:  Rise(h.Swap, h.Swap+StemSwapEdgeSec, at),
		StemBass:   Rise(h.BassAt, h.BassAt+StemSwapEdgeSec, at),
		StemOther:  Rise(math.Max(0, h.Swap-1.2), h.Swap, at),
		StemVocals: Rise(h.VocalIn, h.VocalIn+stemCutSec, at),
	}
}
