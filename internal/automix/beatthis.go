package automix

import (
	"math"
	"sort"
)

// Beat This! 契约中纯算术的两半：模型输入要求的梅尔频谱，以及把两条 logit 轨变成时刻的峰值挑选。
// 模型本身不在这里（见 internal/automix/models），本文件保持纯函数、可测试。
//
// 以下常量全是契约而非参数：检查点训练时的取值（上游 beatThis.ts，参照 mosynthkey/beat_this_cpp）。
// 错一个不会崩溃，只会给出一个自信的错误网格。改动这条链路前须与参考实现对拍。

// BeatThisSampleRate Beat This! 以 22.05kHz 单声道运行，恰好也是离线档案的解码率。
const BeatThisSampleRate = ProfileSampleRate

const (
	btNFFT          = 1024
	btHop           = 441
	btMels          = 128
	btFMin          = 30.0
	btFMax          = 11000.0
	btLogMultiplier = 1000.0
	btAmin          = 1e-10
	// BeatThisFPS 22050 / 441，每帧五十分之一秒。
	BeatThisFPS = float64(BeatThisSampleRate) / btHop
	// BeatThisMelBands 模型输入的第三维。
	BeatThisMelBands = btMels
	// BeatThisChunkFrames 模型一次接受的最多帧数（30 秒）。导出的图拒收更长的张量，
	// 整首喂入会在注意力层报 invalid expand shape，因此分块是必需而非优化。
	BeatThisChunkFrames = 1500
	// btBorderFrames 每块两端丢弃的帧数：边缘帧只有一半上下文。
	btBorderFrames = 6
)

// Slaney 梅尔（torchaudio 默认）：1kHz 以下线性，以上对数。
func btHzToMel(hz float64) float64 {
	const fSp = 200.0 / 3
	if hz < 1000 {
		return hz / fSp
	}
	return 1000/fSp + math.Log(hz/1000)/(math.Log(6.4)/27)
}

func btMelToHz(mel float64) float64 {
	const fSp = 200.0 / 3
	minLogMel := 1000 / fSp
	if mel < minLogMel {
		return fSp * mel
	}
	return 1000 * math.Exp((math.Log(6.4)/27)*(mel-minLogMel))
}

var (
	btFilterbank [][]float64
	btHann       []float64
)

// 三角滤波器在赫兹域里连接梅尔等距的拐点（torchaudio 的做法），不是在梅尔域里对称。
func btBuildFilterbank() [][]float64 {
	bins := btNFFT/2 + 1
	melMin, melMax := btHzToMel(btFMin), btHzToMel(btFMax)
	corners := make([]float64, btMels+2)
	for i := range corners {
		corners[i] = btMelToHz(melMin + (melMax-melMin)*float64(i)/float64(btMels+1))
	}
	fb := make([][]float64, btMels)
	for band := range fb {
		row := make([]float64, bins)
		left, centre, right := corners[band], corners[band+1], corners[band+2]
		for bin := 0; bin < bins; bin++ {
			hz := float64(bin) * BeatThisSampleRate / btNFFT
			rising, falling := 0.0, 0.0
			if centre != left {
				rising = (hz - left) / (centre - left)
			}
			if right != centre {
				falling = (right - hz) / (right - centre)
			}
			row[bin] = math.Max(0, math.Min(rising, falling))
		}
		fb[band] = row
	}
	return fb
}

func btInit() {
	if btFilterbank != nil {
		return
	}
	btFilterbank = btBuildFilterbank()
	// 周期 Hann（STFT 惯例）：分母是 size，不是 size-1。
	btHann = make([]float64, btNFFT)
	for i := range btHann {
		btHann[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/btNFFT))
	}
}

// MelSpectrogram frames × 128，行优先，可直接作为 [1, frames, 128] 交给模型。
type MelSpectrogram struct {
	Data   []float32
	Frames int
}

// BeatThisMel Beat This! 训练时所用的对数梅尔频谱。
//
// 按 torch.stft(center=True, pad_mode='reflect') 居中：两端各反射半个窗口，
// 第 i 帧中心落在采样 i*HOP。少了它每个拍点都晚半个窗口（23ms）。
func BeatThisMel(mono []float32) MelSpectrogram {
	btInit()
	pad := btNFFT / 2
	if len(mono) == 0 {
		return MelSpectrogram{}
	}
	padded := make([]float64, len(mono)+2*pad)
	for i := 0; i < pad; i++ {
		// 反射但不重复端点（numpy 'reflect'，torch 默认）。
		padded[i] = float64(mono[min(pad-i, len(mono)-1)])
		padded[len(padded)-1-i] = float64(mono[max(0, len(mono)-1-(pad-i))])
	}
	for i, v := range mono {
		padded[pad+i] = float64(v)
	}
	frames := (len(padded)-btNFFT)/btHop + 1
	if frames <= 0 {
		return MelSpectrogram{}
	}
	data := make([]float32, frames*btMels)
	real := make([]float64, btNFFT)
	imag := make([]float64, btNFFT)
	magnitude := make([]float64, btNFFT/2+1)
	// torchaudio 按窗口长度的平方根归一化。
	scale := 1 / math.Sqrt(btNFFT)
	for frame := 0; frame < frames; frame++ {
		start := frame * btHop
		for i := 0; i < btNFFT; i++ {
			real[i] = padded[start+i] * btHann[i]
			imag[i] = 0
		}
		FFT(real, imag)
		for bin := range magnitude {
			magnitude[bin] = math.Sqrt(real[bin]*real[bin]+imag[bin]*imag[bin]) * scale
		}
		for band := 0; band < btMels; band++ {
			row := btFilterbank[band]
			var energy float64
			for bin, m := range magnitude {
				energy += m * row[bin]
			}
			data[frame*btMels+band] = float32(math.Log1p(btLogMultiplier * math.Max(energy, btAmin)))
		}
	}
	return MelSpectrogram{Data: data, Frames: frames}
}

// deduplicateFrames 合并相距 width 帧以内的峰，保留其滑动均值。
func deduplicateFrames(peaks []int, width float64) []int {
	if len(peaks) == 0 {
		return nil
	}
	var merged []int
	mean := float64(peaks[0])
	count := 1.0
	for _, p := range peaks[1:] {
		peak := float64(p)
		if peak-mean <= width {
			count++
			mean += (peak - mean) / count
		} else {
			merged = append(merged, int(jsRound(mean)))
			mean = peak
			count = 1
		}
	}
	return append(merged, int(jsRound(mean)))
}

// jsRound 与 JavaScript Math.round 一致（.5 向正无穷取整）。
func jsRound(v float64) float64 { return math.Floor(v + 0.5) }

// BeatsFromLogits Beat This! 自己的后处理：不用 DBN。拍点就是七帧内的局部最大且为正。
// 不要在这里加任何平滑——那会重新引入模型刻意避免的节拍与拍号假设。
func BeatsFromLogits(beatLogits, downbeatLogits []float32, fps float64) BeatGrid {
	pick := func(logits []float32) []int {
		var peaks []int
		for i, value := range logits {
			if !(value > 0) {
				continue
			}
			isPeak := true
			for off := -3; off <= 3 && isPeak; off++ {
				o := i + off
				if o >= 0 && o < len(logits) && logits[o] > value {
					isPeak = false
				}
			}
			if isPeak {
				peaks = append(peaks, i)
			}
		}
		return deduplicateFrames(peaks, 1)
	}
	var grid BeatGrid
	for _, f := range pick(beatLogits) {
		grid.Beats = append(grid.Beats, float64(f)/fps)
	}
	if len(grid.Beats) == 0 {
		return grid
	}
	// 重拍本身就是拍，因此挪到最近的拍上，而不是留在差一两帧的位置。
	seen := map[float64]bool{}
	for _, f := range pick(downbeatLogits) {
		t := float64(f) / fps
		best := grid.Beats[0]
		for _, b := range grid.Beats {
			if math.Abs(b-t) < math.Abs(best-t) {
				best = b
			}
		}
		if !seen[best] {
			seen[best] = true
			grid.Downbeats = append(grid.Downbeats, best)
		}
	}
	sort.Float64s(grid.Downbeats)
	return grid
}

// MelChunk 最多 BeatThisChunkFrames × 128，越过两端之处补零。Start 为本块首行在整曲中的帧号，可为负。
type MelChunk struct {
	Data   []float32
	Frames int
	Start  int
}

// chunkStarts 与参考实现完全一致的分块起点。
func chunkStarts(frames int) []int {
	stride := BeatThisChunkFrames - 2*btBorderFrames
	var starts []int
	for s := -btBorderFrames; s < frames-btBorderFrames; s += stride {
		starts = append(starts, s)
	}
	if len(starts) == 0 {
		return []int{-btBorderFrames}
	}
	// avoid_short_end：最后一块拉回到与曲尾齐平，而不是留一个几乎没有上下文的残块。
	if frames > stride {
		starts[len(starts)-1] = frames - (BeatThisChunkFrames - btBorderFrames)
	}
	return starts
}

// SplitForModel 把频谱切成模型能接受的块。
func SplitForModel(mel MelSpectrogram) []MelChunk {
	if mel.Frames == 0 {
		return nil
	}
	var chunks []MelChunk
	for _, start := range chunkStarts(mel.Frames) {
		from := max(0, start)
		to := min(start+BeatThisChunkFrames, mel.Frames)
		leftPad := max(0, -start)
		rightPad := max(0, min(btBorderFrames, start+BeatThisChunkFrames-mel.Frames))
		frames := leftPad + max(0, to-from) + rightPad
		data := make([]float32, frames*btMels)
		if to > from {
			copy(data[leftPad*btMels:], mel.Data[from*btMels:to*btMels])
		}
		chunks = append(chunks, MelChunk{Data: data, Frames: frames, Start: start})
	}
	return chunks
}

// ChunkPrediction 一块的两条 logit 轨。
type ChunkPrediction struct {
	Start, Frames  int
	Beat, Downbeat []float32
}

// AggregateChunks 把各块拼回整曲每帧一个预测。重叠处前面的块获胜（参考实现的 keep_first）。
func AggregateChunks(predictions []ChunkPrediction, frames int) (beat, downbeat []float32) {
	beat = make([]float32, frames)
	downbeat = make([]float32, frames)
	// 低于峰值挑选的任何阈值，未被任何块覆盖的帧永远不会成为拍。
	for i := range beat {
		beat[i], downbeat[i] = -1000, -1000
	}
	for i := len(predictions) - 1; i >= 0; i-- {
		c := predictions[i]
		wide := c.Frames >= 2*btBorderFrames
		from, to := 0, c.Frames
		if wide {
			from, to = btBorderFrames, c.Frames-btBorderFrames
		}
		for off := from; off < to; off++ {
			target := c.Start + off
			if target < 0 || target >= frames || off >= len(c.Beat) || off >= len(c.Downbeat) {
				continue
			}
			beat[target] = c.Beat[off]
			downbeat[target] = c.Downbeat[off]
		}
	}
	return beat, downbeat
}

// BeatThisRunner 跑一块频谱，返回每帧的拍与重拍 logit。由 models 包的 ONNX 会话实现。
type BeatThisRunner interface {
	RunBeatThis(chunk []float32, frames int) (beat, downbeat []float32, err error)
}

// AnalyseBeatGrid 完整的 Beat This! 流程：采样进，拍点网格出。
func AnalyseBeatGrid(mono []float32, runner BeatThisRunner) (*BeatGrid, error) {
	mel := BeatThisMel(mono)
	chunks := SplitForModel(mel)
	if len(chunks) == 0 {
		return nil, nil
	}
	predictions := make([]ChunkPrediction, len(chunks))
	for i, c := range chunks {
		beat, downbeat, err := runner.RunBeatThis(c.Data, c.Frames)
		if err != nil {
			return nil, err
		}
		predictions[i] = ChunkPrediction{Start: c.Start, Frames: c.Frames, Beat: beat, Downbeat: downbeat}
	}
	beat, downbeat := AggregateChunks(predictions, mel.Frames)
	grid := BeatsFromLogits(beat, downbeat, BeatThisFPS)
	return &grid, nil
}
