package models

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-musicfox/go-musicfox/internal/automix"
	"github.com/go-musicfox/go-musicfox/internal/automix/ort"
)

// htdemucs：44.1kHz 原始立体声进，drums/bass/vocals 三轨出（other 由相减得出）。
//
// 两条路径：
//   - python：folia 的 htdemucs_runner.py 跑在 Python 进程里。只有 ORT 的 Python 绑定能关
//     enable_mem_reuse，它把一次分离从约 2.5GB 压到约 0.5GB；进程退出即归还全部内存。
//   - native：进程内 onnxruntime，关掉 CPU arena 与 mem pattern。没有 Python 时的退路，
//     峰值内存更高（folia 实测 mem_reuse 开着约 0.8–2.5GB，视段长而定），会话用完即释放。
//
// 两条路径的分段、三角窗 overlap-add 与轨序完全一致（与 demucs_onnx 参考实现相同）。

//go:embed htdemucs_runner.py
var runnerScript []byte

// htdemucs 输出行序与返回给调用方的轨。
var (
	demucsSources  = []string{"drums", "bass", "other", "vocals"}
	demucsReturned = []string{"drums", "bass", "vocals"}
)

// maxSeparateSamples 一次最长分离 40 秒（规划器最长重叠 25s 加两侧余量）。
const maxSeparateSamples = 40 * automix.StemSampleRate

// sidecarTimeout 最慢的实测约 17s（40s 窗口加冷启动），这是"没在跑"的界线，不是性能目标。
const sidecarTimeout = 3 * time.Minute

// StemsReady 模型与运行后端都在。
func (e *Engine) StemsReady() bool {
	if e == nil {
		return false
	}
	if _, ok := e.store.HtdemucsModel(); !ok {
		return false
	}
	backend, _ := e.stemBackend()
	return backend != ""
}

// stemBackend 选择分离后端：python / native，都不可用时为空。
func (e *Engine) stemBackend() (string, string) {
	mode := e.store.opts.StemBackend
	if mode != "native" {
		if py, ok := e.store.Python(); ok {
			return "python", py
		}
		if mode == "python" {
			return "", ""
		}
	}
	if lib, ok := e.store.OrtLibrary(); ok {
		return "native", lib
	}
	return "", ""
}

// Separate 分离一段立体声，返回 drums、bass、vocals 三轨（各两声道）。同一时刻只跑一个。
func (e *Engine) Separate(ctx context.Context, left, right []float32) (map[string][2][]float32, error) {
	if len(left) != len(right) || len(left) == 0 || len(left) > maxSeparateSamples {
		return nil, fmt.Errorf("channels of %d and %d samples, want 1..%d", len(left), len(right), maxSeparateSamples)
	}
	model, ok := e.store.HtdemucsModel()
	if !ok {
		return nil, errors.New("htdemucs.onnx not installed")
	}
	e.separating.Lock()
	defer e.separating.Unlock()
	backend, exe := e.stemBackend()
	switch backend {
	case "python":
		return e.separatePython(ctx, exe, model, left, right)
	case "native":
		return e.separateNative(model, left, right)
	default:
		return nil, errors.New("no way to run htdemucs: no Python runtime and no onnxruntime library")
	}
}

func (e *Engine) runnerPath() (string, error) {
	path := e.store.path("htdemucs_runner.py")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, runnerScript) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, runnerScript, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func writeFloats(path string, parts ...[]float32) error {
	var buf bytes.Buffer
	for _, p := range parts {
		if err := binary.Write(&buf, binary.LittleEndian, p); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func (e *Engine) separatePython(ctx context.Context, python, model string, left, right []float32) (map[string][2][]float32, error) {
	script, err := e.runnerPath()
	if err != nil {
		return nil, err
	}
	// 每次一个 0700 的随机目录，而不是共享临时目录里两个可预测的名字（CWE-377）。
	dir, err := os.MkdirTemp("", "musicfox-htd-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "mix.in"), filepath.Join(dir, "stems.out")
	total := len(left)
	if err := writeFloats(in, left, right); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, sidecarTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, script, in, out, strconv.Itoa(total), model)
	hideWindow(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(output.String())
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("htdemucs runner: %w: %s", err, tail)
	}
	// 成功时也打印一行代价：只记失败，正是 4GB 峰值没人量过一整天的原因。
	if msg := strings.TrimSpace(output.String()); msg != "" {
		slog.Info("automix htdemucs", "runner", msg)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	want := len(demucsReturned) * 2 * total
	if len(raw) != want*4 {
		return nil, fmt.Errorf("runner wrote %d floats, expected %d", len(raw)/4, want)
	}
	floats := make([]float32, want)
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, floats); err != nil {
		return nil, err
	}
	result := map[string][2][]float32{}
	for i, name := range demucsReturned {
		base := i * 2 * total
		result[name] = [2][]float32{floats[base : base+total], floats[base+total : base+2*total]}
	}
	return result, nil
}

// buildWindow 参考实现的三角 overlap-add 窗：段间 1/4 重叠，重叠处线性交叉。
func buildWindow(segment int) []float32 {
	overlap := segment / 4
	win := make([]float32, segment)
	for i := range win {
		win[i] = 1
	}
	for i := 0; i < overlap; i++ {
		ramp := float32(i) / float32(overlap-1)
		win[i] = ramp
		win[segment-1-i] = ramp
	}
	return win
}

// overlapAdd 与 htdemucs_runner.separate 相同的分段推理；run 对一段 [2, segment] 返回 [4, 2, segment]。
func overlapAdd(left, right []float32, segment int, run func(in []float32) ([]float32, error)) (map[string][2][]float32, error) {
	total := len(left)
	win := buildWindow(segment)
	stride := segment - segment/4
	chunks := max(1, (total+stride-1)/stride)
	out := make([][2][]float32, len(demucsSources))
	for s := range out {
		out[s] = [2][]float32{make([]float32, total), make([]float32, total)}
	}
	weight := make([]float32, total)
	in := make([]float32, 2*segment)
	for c := 0; c < chunks; c++ {
		start := c * stride
		end := min(start+segment, total)
		if end <= start {
			break
		}
		n := end - start
		clear(in)
		copy(in[:n], left[start:end])
		copy(in[segment:segment+n], right[start:end])
		stems, err := run(in)
		if err != nil {
			return nil, err
		}
		if len(stems) != len(demucsSources)*2*segment {
			return nil, fmt.Errorf("model returned %d values, expected %d", len(stems), len(demucsSources)*2*segment)
		}
		for s := range demucsSources {
			for ch := 0; ch < 2; ch++ {
				row := stems[(s*2+ch)*segment:]
				dst := out[s][ch]
				for i := 0; i < n; i++ {
					dst[start+i] += row[i] * win[i]
				}
			}
		}
		for i := 0; i < n; i++ {
			weight[start+i] += win[i]
		}
	}
	result := map[string][2][]float32{}
	for s, name := range demucsSources {
		for ch := 0; ch < 2; ch++ {
			for i := range out[s][ch] {
				out[s][ch][i] /= float32(math.Max(float64(weight[i]), 1e-8))
			}
		}
		result[name] = out[s]
	}
	delete(result, "other")
	return result, nil
}

func (e *Engine) separateNative(model string, left, right []float32) (map[string][2][]float32, error) {
	e.mu.Lock()
	rt, err := e.runtimeLocked()
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	started := time.Now()
	s, err := rt.NewSession(model, ort.SessionOptions{Threads: e.store.Threads(), DisableCPUArena: true, DisableMemPattern: true})
	if err != nil {
		return nil, fmt.Errorf("load htdemucs: %w", err)
	}
	// 会话用完即释放：分离一首歌之后几分钟内都不再需要它，300MB+ 的权重不该常驻。
	defer s.Close()
	shape, err := s.InputShape(0)
	if err != nil || len(shape) != 3 || shape[1] != 2 || shape[2] <= 0 {
		return nil, fmt.Errorf("unexpected htdemucs input shape %v (%v)", shape, err)
	}
	// 段长向模型要，而不是在这里再写一个常量：两个答案不一致是静默的错音频。
	segment := int(shape[2])
	loaded := time.Since(started)
	result, err := overlapAdd(left, right, segment, func(in []float32) ([]float32, error) {
		out, err := s.Run(ort.Tensor{Shape: []int64{1, 2, int64(segment)}, Data: in})
		if err != nil {
			return nil, err
		}
		return out[0].Data, nil
	})
	if err == nil {
		slog.Info("automix htdemucs", "backend", "native", "segment", segment, "seconds", float64(len(left))/automix.StemSampleRate,
			"load", loaded.Round(time.Millisecond), "total", time.Since(started).Round(time.Millisecond), "threads", e.store.Threads())
	}
	return result, err
}

// SeparateWindow 分离一个窗口并组装成 StemWindow：other = 混音 − 三轨，四轨之和精确等于原曲。
// 曲尾窗口顺带测出最后还在唱的时刻。
func (e *Engine) SeparateWindow(ctx context.Context, left, right []float32, from float64, role automix.StemRole) (*automix.StemWindow, error) {
	parts, err := e.Separate(ctx, left, right)
	if err != nil {
		return nil, err
	}
	n := len(left)
	var stems [automix.StemCount][2][]float32
	stems[automix.StemDrums] = parts["drums"]
	stems[automix.StemBass] = parts["bass"]
	stems[automix.StemVocals] = parts["vocals"]
	// 在浮点里相减、量化之前，拼接点上只剩 -96dBFS 的存储误差。
	other := [2][]float32{make([]float32, n), make([]float32, n)}
	mix := [2][]float32{left, right}
	for ch := 0; ch < 2; ch++ {
		for i := 0; i < n; i++ {
			other[ch][i] = mix[ch][i] - stems[automix.StemDrums][ch][i] - stems[automix.StemBass][ch][i] - stems[automix.StemVocals][ch][i]
		}
	}
	stems[automix.StemOther] = other
	w := automix.NewStemWindow(automix.StemSampleRate, from, role, stems)
	if role == automix.RoleTail {
		vocals := automix.EnvelopeOf([][]float32{parts["vocals"][0], parts["vocals"][1]}, automix.StemSampleRate, automix.StemCellSec)
		// 参照用真正在放的混音，而不是四轨之和。
		env := automix.EnvelopeOf([][]float32{left, right}, automix.StemSampleRate, automix.StemCellSec)
		if end := automix.LastVocalMoment(vocals, env, automix.StemCellSec); end != nil {
			w.VocalEnd = new(float64)
			*w.VocalEnd = from + *end
		}
	}
	return w, nil
}
