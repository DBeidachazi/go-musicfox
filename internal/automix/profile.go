package automix

import (
	"math"
)

// TrackProfileVersion 算法改动时递增，旧档案会被重新测量。
const TrackProfileVersion = 9

// ProfileSampleRate 分析所用采样率。所测内容都在 11kHz 以下。
const ProfileSampleRate = 22050

const (
	fftSize            = 2048
	hop                = 512
	fluxCeilingHz      = 5000
	chromaMinHz        = 65
	chromaMaxHz        = 2000
	silenceBelowPeakDB = 40
	edgeWindowSec      = 10
	hotWindowSec       = 1.5
	hotEdgeDB          = 6
	vocalLowHz         = 300
	vocalHighHz        = 3400
	vocalMinSec        = 0.8
	vocalSmoothSec     = 0.5
	vocalRise          = 0.5
	downbeatCeilingHz  = 150
	endpointChromaSec  = 20
	outroTempoSec      = 30
	headTempoTolerance = 0.1
	chromaMaxFlatness  = 0.28
	noveltyBinSec      = 1
	noveltyHalfKernel  = 4
	noveltyMinKernel   = 2
	noveltyMinSec      = 2
	noveltySigmas      = 1
)

// KeyEstimate 调性估计。Key 为音级（0 = C），-1 表示无答案。
type KeyEstimate struct {
	Key        int     `json:"key"`
	Major      bool    `json:"major"`
	Confidence float64 `json:"confidence"`
}

// TrackProfile 一首歌的离线测量结果，过渡决策所需的全部事实。
// 字段含义与上游 trackProfile.ts 一致；指针字段为 nil 表示"不知道"。
type TrackProfile struct {
	Version  int     `json:"version"`
	Partial  bool    `json:"partial"`
	Gridless bool    `json:"gridless"`
	Duration float64 `json:"duration"`
	LeadIn   float64 `json:"leadIn"`
	// VocalStart 首个持续的居中人声频段声音。单声道文件为 nil。
	VocalStart *float64 `json:"vocalStart"`
	// SectionStart 第一个结构边界（Foote novelty）。
	SectionStart *float64  `json:"sectionStart"`
	Sections     []float64 `json:"sections"`
	LeadOut      *float64  `json:"leadOut"`
	// BodyOut 从不再保持电平处到文件末尾的秒数（衰减 + 静音）。
	BodyOut    *float64  `json:"bodyOut"`
	StartsHot  bool      `json:"startsHot"`
	EndsHot    *bool     `json:"endsHot"`
	IntroSlope float64   `json:"introSlope"`
	OutroSlope *float64  `json:"outroSlope"`
	Loudness   float64   `json:"loudness"`
	HeadDB     float64   `json:"headDb"`
	TailDB     *float64  `json:"tailDb"`
	IntroTone  []float64 `json:"introTone"`
	OutroTone  []float64 `json:"outroTone"`
	BPM        *float64  `json:"bpm"`
	OutroBPM   *float64  `json:"outroBpm"`
	BeatOffset float64   `json:"beatOffset"`
	// DownbeatOffset 自曲首到某个重拍的秒数，基于曲尾锚定的网格。
	DownbeatOffset *float64 `json:"downbeatOffset"`
	// HeadDownbeatOffset 同一小节线，但在曲首直接测量。
	HeadDownbeatOffset *float64     `json:"headDownbeatOffset"`
	BeatsPerBar        int          `json:"beatsPerBar"`
	Key                int          `json:"key"`
	Major              bool         `json:"major"`
	KeyConfidence      float64      `json:"keyConfidence"`
	IntroKey           KeyEstimate  `json:"introKey"`
	OutroKey           *KeyEstimate `json:"outroKey"`
}

// Krumhansl-Schmuckler 调性模板。
var (
	majorProfile = [12]float64{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88}
	minorProfile = [12]float64{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17}
)

func correlate(a, b []float64) float64 {
	var meanA, meanB float64
	for i := range a {
		meanA += a[i]
		meanB += b[i]
	}
	meanA /= float64(len(a))
	meanB /= float64(len(b))
	var top, l2, r2 float64
	for i := range a {
		l, r := a[i]-meanA, b[i]-meanB
		top += l * r
		l2 += l * l
		r2 += r * r
	}
	if l2 > 0 && r2 > 0 {
		return top / math.Sqrt(l2*r2)
	}
	return 0
}

// KeyFromChroma 12 维色度向量对 24 个调模板求相关，取最优并报告赢了多少。
func KeyFromChroma(chroma []float64) KeyEstimate {
	bestKey, bestMajor, bestScore := -1, true, math.Inf(-1)
	runnerUp := math.Inf(-1)
	rotated := make([]float64, 12)
	for rotation := 0; rotation < 12; rotation++ {
		for i := range rotated {
			rotated[i] = chroma[(i+rotation)%12]
		}
		for _, major := range []bool{true, false} {
			tpl := minorProfile
			if major {
				tpl = majorProfile
			}
			score := correlate(rotated, tpl[:])
			if score > bestScore {
				runnerUp = bestScore
				bestKey, bestMajor, bestScore = rotation, major, score
			} else if score > runnerUp {
				runnerUp = score
			}
		}
	}
	if bestKey < 0 || math.IsInf(bestScore, 0) || bestScore <= 0 {
		return KeyEstimate{Key: -1, Major: true}
	}
	margin := 0.0
	if !math.IsInf(runnerUp, 0) {
		margin = math.Max(0, bestScore-runnerUp)
	}
	return KeyEstimate{
		Key:        bestKey,
		Major:      bestMajor,
		Confidence: math.Min(1, math.Max(0, bestScore)*math.Min(1, margin/0.15)),
	}
}

func smooth(values []float64, window int) []float64 {
	half := max(0, window/2)
	out := make([]float64, len(values))
	if half == 0 {
		copy(out, values)
		return out
	}
	prefix := make([]float64, len(values)+1)
	for i, v := range values {
		prefix[i+1] = prefix[i] + v
	}
	for i := range values {
		from := max(0, i-half)
		to := min(len(values), i+half+1)
		out[i] = (prefix[to] - prefix[from]) / float64(to-from)
	}
	return out
}

func firstSustained(centre []float64, hopSec float64) *float64 {
	if len(centre) == 0 {
		return nil
	}
	curve := smooth(centre, int(math.Round(vocalSmoothSec/hopSec)))
	low, high := math.Inf(1), math.Inf(-1)
	for _, v := range curve {
		low = math.Min(low, v)
		high = math.Max(high, v)
	}
	if !(high-low > 0.02) {
		return nil
	}
	threshold := low + (high-low)*vocalRise
	needed := max(1, int(math.Round(vocalMinSec/hopSec)))
	run := 0
	for i, v := range curve {
		if v <= threshold {
			run = 0
			continue
		}
		run++
		if run >= needed {
			return ptr(float64(i-run+1) * hopSec)
		}
	}
	return nil
}

// findBoundaries Foote novelty：自相似矩阵上沿对角线滑动高斯加权棋盘核，找段落边界。
func findBoundaries(bins [][12]float64, binSec float64) []float64 {
	half := noveltyHalfKernel
	n := len(bins)
	if n < half*2+3 {
		return []float64{}
	}
	unit := make([][12]float64, n)
	for i, bin := range bins {
		var energy float64
		for _, v := range bin {
			energy += v * v
		}
		scale := 0.0
		if energy > 0 {
			scale = 1 / math.Sqrt(energy)
		}
		for k, v := range bin {
			unit[i][k] = v * scale
		}
	}
	similarity := func(a, b int) float64 {
		var dot float64
		for k := 0; k < 12; k++ {
			dot += unit[a][k] * unit[b][k]
		}
		return dot
	}
	novelty := make([]float64, n)
	first := noveltyMinKernel
	last := n - 1 - noveltyMinKernel
	sigma2 := 2 * math.Pow(float64(half)/2, 2)
	for centre := first; centre <= last; centre++ {
		reach := min(half, centre, n-1-centre)
		var score, mass float64
		for a := -reach; a < reach; a++ {
			for b := -reach; b < reach; b++ {
				sign := -1.0
				if (a < 0) == (b < 0) {
					sign = 1
				}
				taper := math.Exp(-float64(a*a+b*b) / sigma2)
				score += sign * taper * similarity(centre+a, centre+b)
				mass += taper
			}
		}
		if mass > 0 {
			novelty[centre] = score / mass
		}
	}
	scored := novelty[half : n-half]
	lo, hi := math.Inf(1), math.Inf(-1)
	var mean float64
	for _, v := range scored {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
		mean += v
	}
	if hi-lo < 1e-6 {
		return []float64{}
	}
	mean /= float64(len(scored))
	var variance float64
	for _, v := range scored {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(scored))
	threshold := mean + noveltySigmas*math.Sqrt(variance)

	lean := func(i int) float64 {
		left, right := novelty[i-1], novelty[i+1]
		curve := left - 2*novelty[i] + right
		if !(math.Abs(curve) > 1e-12) {
			return 0
		}
		return math.Max(-0.5, math.Min(0.5, (left-right)/(2*curve)))
	}
	earliest := max(first+1, int(math.Ceil(noveltyMinSec/binSec)))
	boundaries := []float64{}
	for i := earliest; i < last; i++ {
		if novelty[i] > novelty[i-1] && novelty[i] >= novelty[i+1] && novelty[i] > threshold {
			boundaries = append(boundaries, (float64(i)+lean(i))*binSec)
		}
	}
	return boundaries
}

func slopePerSec(values []float64, hopSec float64) float64 {
	count := len(values)
	if count < 2 {
		return 0
	}
	meanX := float64(count-1) / 2
	var meanY float64
	for _, v := range values {
		meanY += v
	}
	meanY /= float64(count)
	var top, bottom float64
	for i, v := range values {
		dx := float64(i) - meanX
		top += dx * (v - meanY)
		bottom += dx * dx
	}
	if bottom > 0 {
		return top / bottom / hopSec
	}
	return 0
}

// TrackEdges 只由电平包络决定的那一半档案。
type TrackEdges struct {
	LeadIn      float64
	SoundingEnd float64
	// BodyEnd 不再保持电平之处，早于 SoundingEnd。
	BodyEnd    float64
	StartsHot  bool
	EndsHot    bool
	IntroSlope float64
	OutroSlope float64
	Loudness   float64
	HeadDB     float64
	TailDB     float64
}

func meanOf(values []float64) float64 {
	var s float64
	for _, v := range values {
		s += v
	}
	return s / float64(len(values))
}

// MeasureEdges 由逐帧电平（dBFS）测量两端。
func MeasureEdges(levels []float64, hopSec, frameSec float64) *TrackEdges {
	if len(levels) == 0 || !(hopSec > 0) {
		return nil
	}
	peak := SilenceDB
	for _, l := range levels {
		peak = math.Max(peak, l)
	}
	if peak <= SilenceDB {
		return nil
	}
	floor := peak - silenceBelowPeakDB
	first, last := -1, -1
	for i, l := range levels {
		if l > floor {
			first = i
			break
		}
	}
	for i := len(levels) - 1; i >= 0; i-- {
		if levels[i] > floor {
			last = i
			break
		}
	}
	if first < 0 || last < first {
		return nil
	}
	sounding := levels[first : last+1]
	edgeFrames := max(2, int(math.Round(edgeWindowSec/hopSec)))
	hotFrames := max(2, int(math.Round(hotWindowSec/hopSec)))
	average := meanOf(sounding)
	head := func(frames int) []float64 { return sounding[:min(frames, len(sounding))] }
	tail := func(frames int) []float64 { return sounding[max(0, len(sounding)-frames):] }

	prefix := make([]float64, len(sounding)+1)
	for i, v := range sounding {
		prefix[i+1] = prefix[i] + v
	}
	holdsLevelAt := func(end int) bool {
		from := max(0, end-hotFrames)
		return (prefix[end]-prefix[from])/float64(end-from) > average-hotEdgeDB
	}
	body := len(sounding)
	for body > 1 && !holdsLevelAt(body) {
		body--
	}

	var energy float64
	counted := 0
	for _, l := range levels {
		if l <= SilenceDB {
			continue
		}
		energy += math.Pow(10, l/10)
		counted++
	}
	integrated := func(values []float64) float64 {
		if len(values) == 0 {
			return SilenceDB
		}
		var s float64
		for _, v := range values {
			s += math.Pow(10, v/10)
		}
		return math.Max(SilenceDB, 10*math.Log10(s/float64(len(values)))+LufsOffsetDB)
	}
	loudness := SilenceDB
	if counted > 0 {
		loudness = 10*math.Log10(energy/float64(counted)) + LufsOffsetDB
	}
	return &TrackEdges{
		LeadIn:      float64(first) * hopSec,
		SoundingEnd: float64(last)*hopSec + frameSec,
		BodyEnd:     math.Max(0, float64(first+body-1)*hopSec+frameSec),
		StartsHot:   meanOf(head(hotFrames)) > average-hotEdgeDB,
		EndsHot:     meanOf(tail(hotFrames)) > average-hotEdgeDB,
		IntroSlope:  slopePerSec(head(edgeFrames), hopSec),
		OutroSlope:  slopePerSec(tail(edgeFrames), hopSec),
		Loudness:    loudness,
		HeadDB:      integrated(head(edgeFrames)),
		TailDB:      integrated(tail(edgeFrames)),
	}
}

// AnalyseOptions AnalyseTrack 的可选输入。
type AnalyseOptions struct {
	// Partial 这些采样只是文件开头，所有关于结尾的字段保持 nil。
	Partial bool
	// Side (L-R)/2。单声道文件为 nil，此时找不到人声。
	Side []float32
	// Grid Beat This! 的拍点，有则整体替换六个网格字段。
	Grid *BeatGrid
	// Gridless 模型根本没运行（与运行了但没答案区分）。
	Gridless bool
}

// AnalyseTrack 一遍扫描解码后的单声道采样，产出过渡需要知道的全部事实。
func AnalyseTrack(mono []float32, sampleRate float64, opts AnalyseOptions) *TrackProfile {
	duration := float64(len(mono)) / sampleRate
	if !(duration > 1) || !(sampleRate > 0) {
		return nil
	}
	hopSec := hop / sampleRate
	bins := fftSize / 2
	binHz := sampleRate / fftSize
	fluxBins := max(8, int(math.Round(fluxCeilingHz/binHz)))
	lowFluxBins := max(4, int(math.Round(downbeatCeilingHz/binHz)))
	toneEdgeBins := [2]int{
		min(bins, int(math.Round(ToneEdgeHz[0]/binHz))),
		min(bins, int(math.Round(ToneEdgeHz[1]/binHz))),
	}
	weighted := KWeight(mono, sampleRate)
	real := make([]float64, fftSize)
	imag := make([]float64, fftSize)
	spectrum := make([]float32, bins)
	previous := make([]float32, bins)

	window := make([]float64, fftSize)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(fftSize-1))
	}
	binPitchClass := make([]int8, bins)
	var chromaBinList []int
	for i := 0; i < bins; i++ {
		hz := float64(i) * binHz
		if hz >= chromaMinHz && hz <= chromaMaxHz {
			pc := (int(math.Round(12*math.Log2(hz/440)+69))%12 + 12) % 12
			binPitchClass[i] = int8(pc)
			chromaBinList = append(chromaBinList, i)
		} else {
			binPitchClass[i] = -1
		}
	}
	magnitudes := make([]float64, bins)

	side := opts.Side
	if len(side) < len(mono) {
		side = nil
	}
	vocalLowBin := max(1, int(math.Floor(vocalLowHz/binHz)))
	vocalHighBin := min(bins-1, int(math.Ceil(vocalHighHz/binHz)))
	var sideReal, sideImag []float64
	if side != nil {
		sideReal = make([]float64, fftSize)
		sideImag = make([]float64, fftSize)
	}

	var levels, envelope, lowEnvelope, centre, chromaEnvelope []float64
	chroma := make([]float64, 12)
	binCount := max(1, int(math.Ceil(duration/noveltyBinSec)))
	sectionBins := make([][12]float64, binCount)
	toneBins := make([][3]float64, binCount)
	var frameChroma, previousChroma [12]float64
	frames := 0

	for start := 0; start+fftSize <= len(mono); start += hop {
		levels = append(levels, RmsDB(weighted[start:start+fftSize]))
		for i := 0; i < fftSize; i++ {
			real[i] = float64(mono[start+i]) * window[i]
			imag[i] = 0
		}
		FFT(real, imag)

		binIndex := min(binCount-1, int(math.Floor(float64(start)/sampleRate/noveltyBinSec)))
		sectionBin := &sectionBins[binIndex]
		toneBin := &toneBins[binIndex]

		var midBand, peak float64
		for i := 0; i < bins; i++ {
			magnitude := math.Hypot(real[i], imag[i])
			magnitudes[i] = magnitude
			if magnitude > 0 {
				spectrum[i] = float32(math.Max(SilenceDB, 20*math.Log10(magnitude)))
			} else {
				spectrum[i] = SilenceDB
			}
			power := magnitude * magnitude
			switch {
			case i < toneEdgeBins[0]:
				toneBin[0] += power
			case i < toneEdgeBins[1]:
				toneBin[1] += power
			default:
				toneBin[2] += power
			}
			if i >= vocalLowBin && i <= vocalHighBin {
				midBand += power
			}
			if binPitchClass[i] >= 0 && magnitude > peak {
				peak = magnitude
			}
		}

		// 谱平坦度：越像噪声，对色度的贡献越小。
		floor := peak * 1e-3
		var arithmetic, logSum float64
		for _, i := range chromaBinList {
			m := math.Max(magnitudes[i], floor)
			arithmetic += m
			logSum += math.Log(m)
		}
		flatness := 1.0
		if arithmetic > 0 && len(chromaBinList) > 0 {
			nb := float64(len(chromaBinList))
			flatness = math.Exp(logSum/nb) / (arithmetic / nb)
		}
		tonality := math.Max(0, 1-flatness/chromaMaxFlatness)
		frameChroma = [12]float64{}
		if tonality > 0 {
			for _, i := range chromaBinList {
				c := magnitudes[i] * tonality
				pc := binPitchClass[i]
				chroma[pc] += c
				sectionBin[pc] += c
				frameChroma[pc] += c
			}
		}

		var harmonicChange, chromaSum float64
		for _, v := range frameChroma {
			chromaSum += v
		}
		if chromaSum > 0 {
			for pc := 0; pc < 12; pc++ {
				share := frameChroma[pc] / chromaSum
				harmonicChange += math.Max(0, share-previousChroma[pc])
				frameChroma[pc] = share
			}
			previousChroma = frameChroma
		}

		if side != nil {
			for i := 0; i < fftSize; i++ {
				sideReal[i] = float64(side[start+i]) * window[i]
				sideImag[i] = 0
			}
			FFT(sideReal, sideImag)
			var sideBand float64
			for i := vocalLowBin; i <= vocalHighBin; i++ {
				sideBand += sideReal[i]*sideReal[i] + sideImag[i]*sideImag[i]
			}
			total := midBand + sideBand
			if total > 0 {
				centre = append(centre, midBand/total)
			} else {
				centre = append(centre, 0.5)
			}
		}

		if frames > 0 {
			envelope = append(envelope, SpectralFlux(spectrum, previous, fluxBins))
			lowEnvelope = append(lowEnvelope, SpectralFlux(spectrum, previous, lowFluxBins))
			chromaEnvelope = append(chromaEnvelope, harmonicChange)
		}
		copy(previous, spectrum)
		frames++
	}

	edges := MeasureEdges(levels, hopSec, fftSize/sampleRate)
	if edges == nil {
		return nil
	}

	var measured *GridFields
	if opts.Grid != nil {
		measured = GridFromBeats(*opts.Grid, opts.Partial)
	}

	tempo := EstimateTempo(envelope, hopSec)
	var beatOffset float64
	if tempo != nil && tempo.PeriodSec > 0 {
		lastBeatSec := float64(len(envelope)-1-tempo.BeatOffsetHops)*hopSec + fftSize/(2*sampleRate)
		beatOffset = modulo(lastBeatSec, tempo.PeriodSec)
	}
	key := KeyFromChroma(chroma)
	partial := opts.Partial

	envelopeStartSec := fftSize / (2 * sampleRate)
	barSec := 0.0
	if tempo != nil && tempo.PeriodSec > 0 {
		barSec = tempo.PeriodSec * BeatsPerBar
	}
	var downbeat *float64
	if measured == nil && tempo != nil && barSec > 0 {
		downbeat = EstimateDownbeat(lowEnvelope, hopSec, tempo.PeriodSec,
			modulo(beatOffset-envelopeStartSec, tempo.PeriodSec), BeatsPerBar, chromaEnvelope)
	}

	tempoFrames := int(math.Round(outroTempoSec / hopSec))
	var outroTempo *TempoEstimate
	if !partial && len(envelope) > tempoFrames {
		outroTempo = EstimateTempo(envelope[len(envelope)-tempoFrames:], hopSec)
	}
	var headTempo *TempoEstimate
	if tempo != nil && len(envelope) > tempoFrames {
		headTempo = EstimateTempo(envelope[:tempoFrames], hopSec)
	}
	var headDownbeat *float64
	if measured == nil && tempo != nil && headTempo != nil &&
		math.Abs(headTempo.PeriodSec/tempo.PeriodSec-1) <= headTempoTolerance {
		headDownbeat = EstimateDownbeat(lowEnvelope[:tempoFrames], hopSec, headTempo.PeriodSec,
			modulo(float64(tempoFrames-1-headTempo.BeatOffsetHops)*hopSec, headTempo.PeriodSec),
			BeatsPerBar, chromaEnvelope[:tempoFrames])
	}

	firstBin := max(0, int(math.Floor(edges.LeadIn/noveltyBinSec)))
	lastBin := min(binCount, int(math.Ceil(edges.SoundingEnd/noveltyBinSec)))
	chromaOver := func(from, to int) KeyEstimate {
		total := make([]float64, 12)
		for i := max(0, from); i < min(binCount, to); i++ {
			for pc := 0; pc < 12; pc++ {
				total[pc] += sectionBins[i][pc]
			}
		}
		return KeyFromChroma(total)
	}
	toneOver := func(from, to int) []float64 {
		var total [3]float64
		for i := max(0, from); i < min(binCount, to); i++ {
			for b := 0; b < 3; b++ {
				total[b] += toneBins[i][b]
			}
		}
		all := total[0] + total[1] + total[2]
		if all > 0 {
			return []float64{total[0] / all, total[1] / all, total[2] / all}
		}
		return []float64{1.0 / 3, 1.0 / 3, 1.0 / 3}
	}
	endpointBins := int(endpointChromaSec / noveltyBinSec)
	toneWindow := int(edgeWindowSec / noveltyBinSec)
	sections := findBoundaries(sectionBins, noveltyBinSec)

	p := &TrackProfile{
		Version:       TrackProfileVersion,
		Partial:       partial,
		Gridless:      opts.Gridless,
		Duration:      duration,
		LeadIn:        edges.LeadIn,
		Sections:      sections,
		StartsHot:     edges.StartsHot,
		IntroSlope:    edges.IntroSlope,
		Loudness:      edges.Loudness,
		HeadDB:        edges.HeadDB,
		IntroTone:     toneOver(firstBin, firstBin+toneWindow),
		BeatOffset:    beatOffset,
		BeatsPerBar:   BeatsPerBar,
		Key:           key.Key,
		Major:         key.Major,
		KeyConfidence: key.Confidence,
		IntroKey:      chromaOver(firstBin, firstBin+endpointBins),
	}
	if side != nil {
		p.VocalStart = firstSustained(centre, hopSec)
	}
	if len(sections) > 0 {
		p.SectionStart = ptr(sections[0])
	}
	if !partial {
		p.LeadOut = ptr(math.Max(0, duration-edges.SoundingEnd))
		p.BodyOut = ptr(math.Max(0, duration-edges.BodyEnd))
		p.EndsHot = ptr(edges.EndsHot)
		p.OutroSlope = ptr(edges.OutroSlope)
		p.TailDB = ptr(edges.TailDB)
		p.OutroTone = toneOver(lastBin-toneWindow, lastBin)
		k := chromaOver(lastBin-endpointBins, lastBin)
		p.OutroKey = &k
	}
	if measured != nil {
		p.BPM = measured.BPM
		p.OutroBPM = measured.OutroBPM
		p.BeatOffset = measured.BeatOffset
		p.DownbeatOffset = measured.DownbeatOffset
		p.HeadDownbeatOffset = measured.HeadDownbeatOffset
		p.BeatsPerBar = measured.BeatsPerBar
	} else {
		if tempo != nil {
			p.BPM = ptr(tempo.BPM)
		}
		if outroTempo != nil {
			p.OutroBPM = ptr(outroTempo.BPM)
		}
		if downbeat != nil {
			p.DownbeatOffset = ptr(modulo(*downbeat+envelopeStartSec, barSec))
		}
		if headDownbeat != nil {
			p.HeadDownbeatOffset = ptr(modulo(*headDownbeat+envelopeStartSec, barSec))
		}
	}
	return p
}
