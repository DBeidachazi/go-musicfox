package models

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-musicfox/go-musicfox/internal/automix"
	"github.com/go-musicfox/go-musicfox/internal/automix/ort"
)

// Engine 已加载的推理后端：进程内的 onnxruntime（Beat This!，以及无 Python 时的 htdemucs）。
//
// Beat This! 常驻：它的答案有截止期（过渡计划的一半取决于它），重载只省约 100MB。
type Engine struct {
	store *Store

	mu      sync.Mutex
	rt      *ort.Runtime
	rtErr   error
	beat    *ort.Session
	beatErr error

	// separating 保证同一时刻只有一个分离（一个 sidecar 或一个原生会话）：
	// 线程池已按"留出机器"定好大小，两个并发会翻倍占用并同时持有两份权重。
	separating sync.Mutex
}

// NewEngine 构造，懒加载。
func NewEngine(store *Store) *Engine { return &Engine{store: store} }

// Store 模型目录。
func (e *Engine) Store() *Store { return e.store }

func (e *Engine) runtimeLocked() (*ort.Runtime, error) {
	if e.rt != nil || e.rtErr != nil {
		return e.rt, e.rtErr
	}
	lib, ok := e.store.OrtLibrary()
	if !ok {
		// 不锁存：库可能正在下载，下次再试。
		return nil, errors.New("onnxruntime library not installed")
	}
	e.rt, e.rtErr = ort.Load(lib)
	if e.rtErr == nil {
		slog.Info("automix: onnxruntime loaded", "version", e.rt.Version, "path", lib)
	} else {
		slog.Warn("automix: onnxruntime failed to load", "path", lib, "error", e.rtErr)
	}
	return e.rt, e.rtErr
}

// BeatThisReady 模型与运行库都在，可以运行 Beat This!。
func (e *Engine) BeatThisReady() bool {
	if e == nil {
		return false
	}
	_, model := e.store.BeatThisModel()
	_, lib := e.store.OrtLibrary()
	e.mu.Lock()
	defer e.mu.Unlock()
	return model && lib && e.beatErr == nil && e.rtErr == nil
}

func (e *Engine) beatSession() (*ort.Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.beat != nil || e.beatErr != nil {
		return e.beat, e.beatErr
	}
	path, ok := e.store.BeatThisModel()
	if !ok {
		return nil, errors.New("beat_this.onnx not installed")
	}
	rt, err := e.runtimeLocked()
	if err != nil {
		// 库缺失不锁存（可能正在下载）；加载失败由 rtErr 自己锁存。
		return nil, err
	}
	started := time.Now()
	s, err := rt.NewSession(path, ort.SessionOptions{Threads: e.store.Threads()})
	if err == nil {
		err = checkBeatThisIO(s)
		if err != nil {
			s.Close()
			s = nil
		}
	}
	if err != nil {
		// 锁存：同一个文件加载失败，重试也不会成功。
		e.beatErr = fmt.Errorf("beat_this: %w", err)
		slog.Warn("automix: Beat This! unavailable, using the built-in estimator", "error", err)
		return nil, e.beatErr
	}
	slog.Info("automix: Beat This! loaded", "took", time.Since(started).Round(time.Millisecond), "threads", e.store.Threads())
	e.beat = s
	return s, nil
}

// checkBeatThisIO 输入 input_spectrogram，输出 beat 与 downbeat。
func checkBeatThisIO(s *ort.Session) error {
	if in := s.Inputs(); len(in) != 1 || in[0] != "input_spectrogram" {
		return fmt.Errorf("unexpected inputs %v", in)
	}
	has := map[string]bool{}
	for _, o := range s.Outputs() {
		has[o] = true
	}
	if !has["beat"] || !has["downbeat"] {
		return fmt.Errorf("unexpected outputs %v", s.Outputs())
	}
	return nil
}

type beatRunner struct{ s *ort.Session }

// RunBeatThis 实现 automix.BeatThisRunner。
func (b beatRunner) RunBeatThis(chunk []float32, frames int) ([]float32, []float32, error) {
	if frames <= 0 || frames > automix.BeatThisChunkFrames || len(chunk) != frames*automix.BeatThisMelBands {
		// 形状不对在原生侧是崩溃而不是异常，这里先挡住。
		return nil, nil, fmt.Errorf("chunk of %d frames / %d values", frames, len(chunk))
	}
	out, err := b.s.Run(ort.Tensor{Shape: []int64{1, int64(frames), automix.BeatThisMelBands}, Data: chunk})
	if err != nil {
		return nil, nil, err
	}
	var beat, downbeat []float32
	for i, name := range b.s.Outputs() {
		switch name {
		case "beat":
			beat = out[i].Data
		case "downbeat":
			downbeat = out[i].Data
		}
	}
	if len(beat) != frames || len(downbeat) != frames {
		return nil, nil, fmt.Errorf("model returned %d/%d frames for %d", len(beat), len(downbeat), frames)
	}
	return beat, downbeat, nil
}

// BeatGrid 对 22050Hz 单声道跑 Beat This!。模型不可用时返回 (nil, nil)：调用方退回自相关估计。
func (e *Engine) BeatGrid(mono []float32) (*automix.BeatGrid, error) {
	if e == nil {
		return nil, nil
	}
	if _, ok := e.store.BeatThisModel(); !ok {
		return nil, nil
	}
	s, err := e.beatSession()
	if err != nil {
		return nil, nil
	}
	return automix.AnalyseBeatGrid(mono, beatRunner{s})
}
