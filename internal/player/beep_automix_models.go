package player

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gopxl/beep"

	"github.com/go-musicfox/go-musicfox/internal/automix"
	"github.com/go-musicfox/go-musicfox/internal/automix/models"
	"github.com/go-musicfox/go-musicfox/internal/configs"
	"github.com/go-musicfox/go-musicfox/utils/app"
)

// automix 的两个可选模型（L1 Beat This!、L2 htdemucs）在播放器侧的接入。
//
// 引擎是进程级的：onnxruntime 一个进程只加载一次，换播放器实例也不重载。

var (
	modelsOnce   sync.Once
	modelsEngine *models.Engine
)

// automixEngine 配置了模型时返回引擎（并在后台补齐下载），否则为 nil。
func automixEngine() *models.Engine {
	modelsOnce.Do(func() {
		cfg := configs.AppConfig.Player.Beep
		level := models.ParseLevel(strings.ToLower(strings.TrimSpace(cfg.AutomixModels)))
		if AutomixSettings().Mode != automix.ModeAutomix || level == models.LevelNone {
			return
		}
		dir := cfg.AutomixModelsDir
		if dir == "" {
			dir = filepath.Join(app.DataDir(), "automix", "models")
		}
		store := models.NewStore(models.Options{
			Dir: dir, Level: level, Mirrors: cfg.AutomixModelMirrors,
			Python: cfg.AutomixPython, OrtLibrary: cfg.AutomixOrtLibrary,
			StemBackend: strings.ToLower(strings.TrimSpace(cfg.AutomixStemBackend)), Threads: cfg.AutomixThreads,
		})
		modelsEngine = models.NewEngine(store)
		slog.Info("automix models", "level", level, "dir", dir, "status", store.Status())
		store.EnsureAsync(context.Background(), nil)
	})
	return modelsEngine
}

// beatGridFor 对 22050Hz 单声道跑 Beat This!。返回的 gridless 表示模型根本没运行。
func beatGridFor(engine *models.Engine, mono []float32) (grid *automix.BeatGrid, gridless bool) {
	if !engine.BeatThisReady() {
		return nil, true
	}
	started := time.Now()
	grid, err := engine.BeatGrid(mono)
	if err != nil {
		slog.Warn("automix Beat This! failed, using the built-in estimator", "error", err)
		return nil, true
	}
	if grid == nil {
		return nil, true
	}
	slog.Info("automix Beat This!", "beats", len(grid.Beats), "downbeats", len(grid.Downbeats),
		"took", time.Since(started).Round(time.Millisecond))
	return grid, false
}

type stemKey struct {
	id   int64
	role automix.StemRole
}

// automixStemCacheSize 最多保留的窗口：够放"即将使用的一对 + 刚用完的一对"。
const automixStemCacheSize = 4

func (a *automixState) stemWindow(id int64, role automix.StemRole) *automix.StemWindow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stems[stemKey{id, role}]
}

// stemsPending 这一对里还有窗口正在分离。
func (a *automixState) stemsPending(from, to int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.separating[stemKey{from, automix.RoleTail}] || a.separating[stemKey{to, automix.RoleHead}]
}

func (a *automixState) storeStems(k stemKey, w *automix.StemWindow) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.separating, k)
	if w == nil {
		return
	}
	if _, ok := a.stems[k]; !ok {
		a.stemOrder = append(a.stemOrder, k)
	}
	a.stems[k] = w
	for len(a.stemOrder) > automixStemCacheSize {
		delete(a.stems, a.stemOrder[0])
		a.stemOrder = a.stemOrder[1:]
	}
}

// claimStems 占下一个待分离的窗口；已有、正在分离或模型不可用时返回 false。
// 同步占位，使规划器在分离真正开始之前就知道要等它。
func (a *automixState) claimStems(id int64, role automix.StemRole) bool {
	if a.engine == nil || !a.engine.StemsReady() || id == 0 {
		return false
	}
	k := stemKey{id, role}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stems[k] != nil || a.separating[k] {
		return false
	}
	a.separating[k] = true
	return true
}

// ensureStems 占位并分离。同步执行，调用方负责放在后台；path 在返回前须保持可读。
func (a *automixState) ensureStems(music URLMusic, path string, role automix.StemRole, outputRate beep.SampleRate) {
	if a.claimStems(music.Id, role) {
		a.separateStems(music, path, role, outputRate)
	}
}

// separateStems 分离一首歌的曲首或曲尾窗口，窗口须已由 claimStems 占下。
func (a *automixState) separateStems(music URLMusic, path string, role automix.StemRole, outputRate beep.SampleRate) {
	engine := a.engine
	k := stemKey{music.Id, role}
	started := time.Now()
	left, right, from, err := decodeStemWindow(path, music, role)
	if err != nil {
		a.storeStems(k, nil)
		slog.Info("automix skips separation", "song_id", music.Id, "role", role, "reason", err)
		return
	}
	decoded := time.Since(started)
	w, err := engine.SeparateWindow(context.Background(), left, right, from, role)
	if err != nil {
		a.storeStems(k, nil)
		slog.Warn("automix separation failed", "song_id", music.Id, "role", role, "error", err)
		return
	}
	w.PrepareEnvelopes()
	if outputRate > 0 && float64(outputRate) != w.Rate {
		w = w.Resampled(float64(outputRate))
	}
	a.storeStems(k, w)
	sung := ""
	if role == automix.RoleTail {
		if w.VocalEnd == nil {
			sung = ", no singing in the tail"
		} else {
			sung = fmt.Sprintf(", still singing at %.2fs", *w.VocalEnd)
		}
	}
	slog.Info("automix separated", "song_id", music.Id, "name", music.Name, "role", role,
		"window", fmt.Sprintf("%.1f-%.1fs%s", from, from+float64(len(left))/automix.StemSampleRate, sung),
		"decode", decoded.Round(time.Millisecond), "total", time.Since(started).Round(time.Millisecond))
}

// decodeStemWindow 用播放同一套解码器取出曲首/曲尾 30 秒的 44.1kHz 立体声。
//
// 必须与播放解码一致：minimp3 与 go-mp3 的起点相差约 25ms，四轨与整轨在拼接处错开这么多就是一声回音。
func decodeStemWindow(path string, music URLMusic, role automix.StemRole) (left, right []float32, from float64, err error) {
	samples, rate, start, err := readStemWindow(path, music, role)
	if err != nil {
		return nil, nil, 0, err
	}
	var stream beep.Streamer = beep.StreamerFunc(func(dst [][2]float64) (int, bool) {
		n := copy(dst, samples)
		samples = samples[n:]
		return n, n > 0
	})
	if rate != automix.StemSampleRate {
		stream = beep.Resample(3, rate, automix.StemSampleRate, stream)
	}
	buf := make([][2]float64, 8192)
	for {
		n, ok := stream.Stream(buf)
		for _, s := range buf[:n] {
			left = append(left, float32(s[0]))
			right = append(right, float32(s[1]))
		}
		if !ok || n == 0 {
			break
		}
	}
	if len(left) < automix.StemSampleRate*5 {
		return nil, nil, 0, fmt.Errorf("only %d samples decoded", len(left))
	}
	return left, right, float64(start) / float64(rate), nil
}

// stemSeekPreroll 跳转后丢弃的采样数（四个 MP3 帧）。
const stemSeekPreroll = 4 * 1152

// readStemWindow 以文件原采样率读出窗口，start 为首个采样在曲中的下标。
func readStemWindow(path string, music URLMusic, role automix.StemRole) (samples [][2]float64, rate beep.SampleRate, start int, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	t := music.Type
	if sniffed, ok := sniffFormat(file); ok {
		t = sniffed
	}
	raw, format, err := decodeSong(t, file, music.Duration, true)
	if err != nil {
		_ = file.Close()
		return nil, 0, 0, err
	}
	defer raw.Close()
	window := int(automix.StemWindowSec * float64(format.SampleRate))
	if role == automix.RoleTail {
		total := raw.Len()
		if total <= 0 {
			return nil, 0, 0, fmt.Errorf("stream length unknown")
		}
		start = max(0, total-window)
		// 解码器跳转后头一帧是预热（MP3 为 1152 个采样的错误值）：早跳几帧再丢掉。
		preroll := min(start, stemSeekPreroll)
		if start > 0 {
			if err := raw.Seek(start - preroll); err != nil {
				return nil, 0, 0, err
			}
		}
		buf := make([][2]float64, 4096)
		for preroll > 0 {
			n, ok := raw.Stream(buf[:min(len(buf), preroll)])
			preroll -= n
			if !ok || n == 0 {
				return nil, 0, 0, fmt.Errorf("stream ended during pre-roll")
			}
		}
	}
	samples = make([][2]float64, 0, window)
	buf := make([][2]float64, 8192)
	for len(samples) < window {
		n, ok := raw.Stream(buf[:min(len(buf), window-len(samples))])
		samples = append(samples, buf[:n]...)
		if !ok || n == 0 {
			break
		}
	}
	return samples, format.SampleRate, start, nil
}

// copyToTemp 复制一份文件，供后台分析在原文件被截断或删除后继续读。
func copyToTemp(path, pattern string) (string, error) {
	tmp, err := os.CreateTemp(app.RuntimeDir(), pattern)
	if err != nil {
		return "", err
	}
	src, err := os.Open(path)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	_, err = io.Copy(tmp, src)
	_ = src.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// separatedExtent 规划器看到的分离窗口：只有两端都有时才给，
// "这个过渡能不能长"与"手法会不会运行"必须由同一份证据回答。
func (a *automixState) separatedExtents(from, to int64) (tail, head *automix.StemWindow, fromExt, toExt *automix.Extent) {
	tail, head = a.stemWindow(from, automix.RoleTail), a.stemWindow(to, automix.RoleHead)
	if tail == nil || head == nil {
		return tail, head, nil, nil
	}
	return tail, head,
		&automix.Extent{From: tail.From, To: tail.From + tail.Duration()},
		&automix.Extent{From: head.From, To: head.From + head.Duration()}
}

// planStemGestureNoLock 过渡开始时决定是否走四轨交接（包络已预先算好，这里只切片）。
func (p *beepPlayer) planStemGestureNoLock(plan automix.TransitionPlan, shape automix.StyledBlend, pos float64,
	fromID, toID int64, fromProfile, toProfile *automix.TrackProfile, outputRate beep.SampleRate,
) (*automix.StemGesture, string) {
	a := p.automix
	if a.engine == nil || a.settings.Mode != automix.ModeAutomix {
		return nil, ""
	}
	if shape.Style == automix.StyleBeatCut || shape.Overlap < automix.MinOverlapSec {
		return nil, ""
	}
	tail, head := a.stemWindow(fromID, automix.RoleTail), a.stemWindow(toID, automix.RoleHead)
	if tail == nil || head == nil {
		switch {
		case tail == nil && head == nil:
			return nil, "no stems (neither end is separated yet)"
		case tail == nil:
			return nil, "no stems (the outgoing end is not separated yet)"
		default:
			return nil, "no stems (the incoming end is not separated yet)"
		}
	}
	g, why := automix.PlanStemGesture(automix.StemGestureRequest{
		Out: tail, In: head,
		OutStart: pos + shape.Hold, InStart: plan.InStart + shape.Hold,
		Wall: shape.Overlap, Rate: float64(outputRate),
		From: fromProfile, To: toProfile, KeysClash: plan.Relation == automix.KeyClashing,
	})
	if g == nil {
		return nil, "no stems (" + why + ")"
	}
	return g, ""
}

// describeStemGesture folia 同款的一行：人声怎么走、鼓与贝斯何时换、两个人声之间的空隙。
func describeStemGesture(g *automix.StemGesture) string {
	h := g.Handover
	kind := map[automix.VocalExitKind]string{
		automix.ExitRest: "cut in a rest", automix.ExitRecede: "receding", automix.ExitRelease: "riding a held note out",
	}[h.Exit.Kind]
	var b strings.Builder
	fmt.Fprintf(&b, "voice %s %.2f-%.2fs (quietest half-second %.0fdB under the mix), drums at %.2fs",
		kind, h.Exit.From, h.Exit.To, h.Exit.LoudDB, h.Swap)
	if g.Bars > 0 {
		b.WriteString(" on a bar line")
	}
	fmt.Fprintf(&b, ", bass at %.2fs", h.BassAt)
	if h.DueAt >= g.Wall-1e-6 {
		b.WriteString(", the next track does not sing in this window, so no deadline")
	} else {
		fmt.Fprintf(&b, ", next voice at %.2fs", h.VocalIn)
		if h.VocalIn > h.DueAt+0.005 {
			fmt.Fprintf(&b, " (held back from %.2fs to meet the note)", h.DueAt)
		}
	}
	if gap := h.VocalIn - h.Exit.To; gap > 0 {
		fmt.Fprintf(&b, ", %.2fs with neither voice", gap)
	} else {
		fmt.Fprintf(&b, ", voices overlap by %.2fs", -gap)
	}
	fmt.Fprintf(&b, ", %.2fs of blend after that", g.Wall-math.Max(h.BassAt, h.VocalIn))
	if h.Exit.Held == nil {
		b.WriteString(", not on a held note")
	} else {
		fmt.Fprintf(&b, ", holding a %.2fs note at %.0fdB", h.Exit.Held.To-h.Exit.Held.From, h.Exit.Held.HoldDB)
		if h.Exit.Held.To >= g.Wall-0.05 {
			b.WriteString(", still holding when the window ends")
		} else {
			fmt.Fprintf(&b, ", let go at %.2fs", h.Exit.Held.To)
		}
	}
	return b.String()
}
