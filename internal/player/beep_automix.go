package player

import (
	"encoding/json"
	"errors"
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
	minimp3pkg "github.com/tosone/minimp3"

	"github.com/go-musicfox/go-musicfox/internal/automix"
	"github.com/go-musicfox/go-musicfox/internal/automix/models"
	"github.com/go-musicfox/go-musicfox/internal/configs"
	"github.com/go-musicfox/go-musicfox/utils/app"
)

// AutomixPlayer 由支持歌曲间过渡的播放器实现。
type AutomixPlayer interface {
	// SetAutomixLyrics 告知某首歌最后一句唱完的时刻（秒）。lastSung 为 nil 表示无法得知。
	SetAutomixLyrics(songID int64, lastSung *float64, hasLyrics bool)
}

// AutomixSettings 从配置读取过渡设置。
func AutomixSettings() automix.Settings {
	cfg := configs.AppConfig.Player.Beep
	mode := automix.Mode(strings.ToLower(strings.TrimSpace(cfg.Automix)))
	switch mode {
	case automix.ModeCrossfade, automix.ModeAutomix:
	default:
		mode = automix.ModeOff
	}
	return automix.Settings{Mode: mode, CrossfadeSeconds: float64(cfg.CrossfadeSeconds)}
}

// AutomixEnabled 是否开启了过渡。
func AutomixEnabled() bool { return AutomixSettings().Mode != automix.ModeOff }

type lyricHint struct {
	lastSung  *float64
	hasLyrics bool
}

// activeBlend 正在进行的一次过渡。tail 是出场曲，其资源在过渡结束后才释放。
type activeBlend struct {
	tail      beep.Streamer
	release   func()
	mixer     *automix.Mixer
	tailEnded bool
}

// automixState 过渡的全部状态。
//
// 锁序：p.l 可以在持有时再取 a.mu，反之不行。profiles / hints / analysing / stems 受 a.mu 保护；
// plan / blend / meter / scratch 只在持有 p.l 时访问。
type automixState struct {
	settings automix.Settings
	// engine 可选模型（L1/L2），未配置时为 nil。
	engine *models.Engine

	mu         sync.Mutex
	stems      map[stemKey]*automix.StemWindow
	stemOrder  []stemKey
	separating map[stemKey]bool
	profiles   map[int64]*automix.TrackProfile
	order      []int64
	analysing  map[int64]bool
	failed     map[int64]bool
	hints      map[int64]lyricHint

	planning bool
	plan     *automix.TransitionPlan
	planFrom int64
	planTo   int64
	blend    *activeBlend
	meter    *automix.LevelMeter
	meterAt  beep.SampleRate
	outBuf   [][2]float64
	inBuf    [][2]float64
}

const automixProfileCacheSize = 32

func newAutomixState(settings automix.Settings) *automixState {
	a := &automixState{
		settings:   settings,
		profiles:   make(map[int64]*automix.TrackProfile),
		analysing:  make(map[int64]bool),
		failed:     make(map[int64]bool),
		hints:      make(map[int64]lyricHint),
		stems:      make(map[stemKey]*automix.StemWindow),
		separating: make(map[stemKey]bool),
	}
	if settings.Mode == automix.ModeAutomix {
		a.engine = automixEngine()
	}
	return a
}

func (a *automixState) profile(id int64) (*automix.TrackProfile, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.profiles[id]
	return p, ok || a.failed[id]
}

func (a *automixState) storeProfile(id int64, p *automix.TrackProfile) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.analysing, id)
	if p == nil {
		a.failed[id] = true
		return
	}
	if _, ok := a.profiles[id]; !ok {
		a.order = append(a.order, id)
	}
	a.profiles[id] = p
	for len(a.order) > automixProfileCacheSize {
		delete(a.profiles, a.order[0])
		a.order = a.order[1:]
	}
}

func (a *automixState) hint(id int64) lyricHint {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hints[id]
}

func profileCachePath(id int64) string {
	if id == 0 {
		return ""
	}
	return filepath.Join(app.CacheDir(), "automix", fmt.Sprintf("%d.json", id))
}

func loadCachedProfile(id int64) *automix.TrackProfile {
	path := profileCachePath(id)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p automix.TrackProfile
	if json.Unmarshal(data, &p) != nil || p.Version != automix.TrackProfileVersion {
		return nil
	}
	return &p
}

func saveCachedProfile(id int64, p *automix.TrackProfile) {
	path := profileCachePath(id)
	if path == "" || p == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if data, err := json.Marshal(p); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

// ensureProfile 取得或测量一首歌的档案。同步执行，调用方负责放在后台 goroutine。
func (a *automixState) ensureProfile(music URLMusic, path string) *automix.TrackProfile {
	id := music.Id
	if p, done := a.profile(id); done {
		return p
	}
	a.mu.Lock()
	if a.analysing[id] {
		a.mu.Unlock()
		return nil
	}
	a.analysing[id] = true
	a.mu.Unlock()

	if p := loadCachedProfile(id); p != nil {
		// 模型没跑过的旧档案，在模型就绪后重测一次，换成模型给出的网格。
		if !(p.Gridless && a.engine.BeatThisReady()) {
			a.storeProfile(id, p)
			return p
		}
		slog.Info("automix re-measuring with Beat This!", "song_id", id)
	}
	started := time.Now()
	p, err := analyseFile(path, music, a.engine)
	switch {
	case errors.Is(err, errAnalysisUnsupported):
		slog.Info("automix skips analysis for this format", "song_id", id, "type", music.Type)
	case err != nil:
		slog.Warn("automix analysis failed", "song_id", id, "error", err)
	}
	a.storeProfile(id, p)
	if p != nil {
		saveCachedProfile(id, p)
		slog.Info("automix profile ready", "song_id", id, "name", music.Name,
			"took", time.Since(started).Round(time.Millisecond), "summary", describeProfile(p))
	}
	return p
}

func describeProfile(p *automix.TrackProfile) string {
	f := func(v *float64) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprintf("%.2f", *v)
	}
	key := "-"
	if p.Key >= 0 {
		names := []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}
		key = names[p.Key]
		if !p.Major {
			key += "m"
		}
		key += fmt.Sprintf("(%.2f)", p.KeyConfidence)
	}
	endsHot := "-"
	if p.EndsHot != nil {
		endsHot = fmt.Sprint(*p.EndsHot)
	}
	return fmt.Sprintf("dur=%.1fs bpm=%s outroBpm=%s key=%s lufs=%.1f leadIn=%.2f leadOut=%s bodyOut=%s startsHot=%v endsHot=%s sections=%d firstSection=%s",
		p.Duration, f(p.BPM), f(p.OutroBPM), key, p.Loudness, p.LeadIn, f(p.LeadOut), f(p.BodyOut),
		p.StartsHot, endsHot, len(p.Sections), f(p.SectionStart))
}

// errAnalysisUnsupported 该格式不做离线分析（FLAC 在 Windows 上只有纯 Go 解码，约 5 倍实时，太慢）。
var errAnalysisUnsupported = errors.New("format not analysed")

// analyseFile 完整解码一个本地文件、降为 22050Hz 的单声道 + 侧声道，交给 AnalyseTrack。
func analyseFile(path string, music URLMusic, engine *models.Engine) (*automix.TrackProfile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	t := music.Type
	if sniffed, ok := sniffFormat(file); ok {
		t = sniffed
	}

	var (
		stream beep.Streamer
		rate   beep.SampleRate
	)
	switch t {
	case Mp3:
		// minimp3 一次性同步解码（C），比 go-mp3 快一个数量级。分析不需要 go-mp3 的
		// 精确 gapless 裁剪，差别约 25ms。
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errors.New("empty file")
		}
		dec, pcm, err := minimp3pkg.DecodeFull(data)
		if err != nil {
			return nil, err
		}
		if dec == nil || dec.SampleRate <= 0 || dec.Channels < 1 || len(pcm) == 0 {
			return nil, errors.New("minimp3 decoded nothing")
		}
		stream, rate = int16Streamer(pcm, dec.Channels), beep.SampleRate(dec.SampleRate)
	case Wav, Ogg:
		raw, format, err := decodeSong(t, file, music.Duration, true)
		if err != nil {
			return nil, err
		}
		defer raw.Close()
		stream, rate = raw, format.SampleRate
	default:
		return nil, errAnalysisUnsupported
	}

	if rate != automix.ProfileSampleRate {
		stream = beep.Resample(3, rate, automix.ProfileSampleRate, stream)
	}
	var mono, side []float32
	buf := make([][2]float64, 8192)
	for {
		n, ok := stream.Stream(buf)
		for _, s := range buf[:n] {
			mono = append(mono, float32((s[0]+s[1])/2))
			side = append(side, float32((s[0]-s[1])/2))
		}
		if !ok || n == 0 {
			break
		}
	}
	// 单声道文件：侧声道全零，没有可消去的人声。
	silentSide := true
	for _, s := range side {
		if s != 0 {
			silentSide = false
			break
		}
	}
	opts := automix.AnalyseOptions{}
	opts.Grid, opts.Gridless = beatGridFor(engine, mono)
	if !silentSide {
		opts.Side = side
	}
	p := automix.AnalyseTrack(mono, automix.ProfileSampleRate, opts)
	if p == nil {
		return nil, fmt.Errorf("nothing measurable in %d samples", len(mono))
	}
	return p, nil
}

// int16Streamer 把交错的 16 位小端 PCM 当作 beep 流读出。
func int16Streamer(pcm []byte, channels int) beep.Streamer {
	frame := 2 * channels
	pos := 0
	return beep.StreamerFunc(func(samples [][2]float64) (int, bool) {
		n := 0
		for n < len(samples) && pos+frame <= len(pcm) {
			l := float64(int16(uint16(pcm[pos])|uint16(pcm[pos+1])<<8)) / 32768
			r := l
			if channels > 1 {
				r = float64(int16(uint16(pcm[pos+2])|uint16(pcm[pos+3])<<8)) / 32768
			}
			samples[n] = [2]float64{l, r}
			pos += frame
			n++
		}
		return n, n > 0
	})
}

// SetAutomixLyrics 实现 AutomixPlayer。
func (p *beepPlayer) SetAutomixLyrics(songID int64, lastSung *float64, hasLyrics bool) {
	if p.automix == nil {
		return
	}
	p.automix.mu.Lock()
	p.automix.hints[songID] = lyricHint{lastSung: lastSung, hasLyrics: hasLyrics}
	p.automix.mu.Unlock()
}

// analyseCurrent 当前曲目下载完成后在后台分析它（作为之后的出场曲），并分离它的曲尾。
func (p *beepPlayer) analyseCurrent(path string, music URLMusic) {
	if p.automix == nil || p.automix.settings.Mode != automix.ModeAutomix {
		return
	}
	// 复制一份：beep_playing 会在下一次 Play 时被截断重写。
	tmp, err := copyToTemp(path, "beep_automix_*")
	if err != nil {
		return
	}
	defer os.Remove(tmp)
	p.automix.ensureProfile(music, tmp)
	p.l.Lock()
	rate := p.gaplessOutputRate
	p.l.Unlock()
	p.automix.ensureStems(music, tmp, automix.RoleTail, rate)
}

// analyseNext 下一首预加载完成时：同步测档案（计划要用），曲首与曲尾分离放到后台。
//
// 曲尾也在这里分离：经过渡换上来的歌不走下载路径，analyseCurrent 不会为它运行，
// 不在这里做，连续播放时只有第一首歌有曲尾窗口。曲首先分（几十秒后就要用），曲尾在后（整首歌之后才用）。
func (p *beepPlayer) analyseNext(prepared *preparedGapless) {
	a := p.automix
	a.ensureProfile(prepared.music, prepared.file.Name())
	var roles []automix.StemRole
	for _, role := range []automix.StemRole{automix.RoleHead, automix.RoleTail} {
		if a.claimStems(prepared.music.Id, role) {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return
	}
	// 预加载的临时文件在交出或取消时会被删除，分离要读它一分钟，先复制一份。
	tmp, err := copyToTemp(prepared.file.Name(), "beep_automix_*")
	if err != nil {
		for _, role := range roles {
			a.storeStems(stemKey{prepared.music.Id, role}, nil)
		}
		return
	}
	go func() {
		defer os.Remove(tmp)
		for _, role := range roles {
			a.separateStems(prepared.music, tmp, role, prepared.outputRate)
		}
	}()
}

// beepPlayerSeconds 当前曲目位置与长度（秒），需持有 p.l。
func (p *beepPlayer) positionNoLock() (pos, length float64) {
	if p.curStreamer == nil || p.curFormat.SampleRate == 0 {
		return 0, 0
	}
	rate := float64(p.curFormat.SampleRate)
	pos = float64(p.curStreamer.Position()) / rate
	if n := p.curStreamer.Len(); n > 0 {
		length = float64(n) / rate
	} else {
		length = p.curMusic.Duration.Seconds()
	}
	return
}

// maybePlanNoLock 下一首已就绪且尚无计划时，在后台制定过渡计划。需持有 p.l。
func (p *beepPlayer) maybePlanNoLock() {
	a := p.automix
	if a.planning || a.blend != nil || p.gapless == nil {
		return
	}
	from := p.curMusic.Id
	next, ready := p.gapless.peekReady(from)
	// 每一对 (出场, 进场) 只规划一次：计划被执行或放弃后都不再重来，
	// 否则放弃后的每次回调都会重新规划，并把下一首再往前跳一次。
	if !ready || (a.planFrom == from && a.planTo == next.Id) {
		return
	}
	pos, length := p.positionNoLock()
	if a.settings.Mode == automix.ModeAutomix {
		// 出场曲档案还在分析、或这一对还在分离时稍等，但不能等到过渡该开始的时候。
		_, done := a.profile(from)
		if (!done || a.stemsPending(from, next.Id)) && length-pos > automix.MaxOverlapSec+15 {
			return
		}
	}
	a.planning = true
	music := p.curMusic
	go p.planTransition(music, next, pos, length)
}

func (p *beepPlayer) planTransition(fromMusic, toMusic URLMusic, at, length float64) {
	a := p.automix
	fromProfile, _ := a.profile(fromMusic.Id)
	toProfile, _ := a.profile(toMusic.Id)
	hint := a.hint(fromMusic.Id)
	from := automix.TransitionTrack{Duration: length, LastSung: hint.lastSung, HasLyrics: hint.hasLyrics}
	to := automix.TransitionTrack{Duration: toMusic.Duration.Seconds()}
	if a.settings.Mode == automix.ModeAutomix {
		from.Profile, to.Profile = fromProfile, toProfile
	}
	if to.Profile != nil {
		to.Duration = to.Profile.Duration
	}
	if a.settings.Mode == automix.ModeAutomix {
		tail, _, fromExt, toExt := a.separatedExtents(fromMusic.Id, toMusic.Id)
		from.Separated, to.Separated = fromExt, toExt
		if tail != nil {
			from.VocalEnd = tail.VocalEnd
		}
	}
	var bpm *float64
	if from.Profile != nil {
		bpm = automix.SettledBPM(from.Profile.BPM, from.Profile.OutroBPM)
	}
	plan := automix.PlanForMode(a.settings, from, to, bpm, at)
	slog.Info("automix plan", "from", fromMusic.Name, "to", toMusic.Name, "kind", plan.Kind,
		"out_start", plan.OutStart, "in_start", plan.InStart, "overlap", plan.Overlap, "reason", plan.Reason)

	// 进场曲的起播点：预先解码并丢弃，避免在音频回调里做（FLAC 的 Seek 很慢）。
	if plan.Kind == automix.KindFade && plan.InStart > 0 {
		if !p.gapless.skipPrepared(fromMusic.Id, plan.InStart) {
			plan.InStart = 0
		}
	}

	p.l.Lock()
	defer p.l.Unlock()
	a.planning = false
	if p.curMusic.Id != fromMusic.Id {
		return
	}
	a.planFrom, a.planTo = fromMusic.Id, toMusic.Id
	if plan.Kind == automix.KindFade {
		a.plan = &plan
	}
}

func (a *automixState) resetNoLock() {
	if a.blend != nil {
		a.blend.release()
		a.blend = nil
	}
	a.plan = nil
	a.planFrom, a.planTo = 0, 0
	if a.meter != nil {
		a.meter.Reset()
	}
}

func grow(buf [][2]float64, n int) [][2]float64 {
	if cap(buf) < n {
		return make([][2]float64, n)
	}
	return buf[:n]
}

// automixStream 在过渡相关的时刻接管采样填充。handled=false 时调用方走原有路径。需持有 p.l。
func (p *beepPlayer) automixStream(dst [][2]float64, current beep.Streamer, outputRate beep.SampleRate) (n int, ok, handled bool) {
	a := p.automix
	if a.blend != nil {
		n, ok = p.mixBlendNoLock(dst, current)
		return n, ok, true
	}
	p.maybePlanNoLock()
	plan := a.plan
	if plan == nil || a.planFrom != p.curMusic.Id {
		return 0, false, false
	}
	pos, _ := p.positionNoLock()
	k := int(math.Round((plan.OutStart - pos) * float64(outputRate)))
	if k >= len(dst) {
		return 0, false, false
	}
	if k > 0 {
		n, ok = current.Stream(dst[:k])
		a.feedMeter(dst[:n], outputRate)
		if !ok || n < k {
			return n, ok, true
		}
	}
	k = max(k, 0)
	if !p.startBlendNoLock(current, outputRate) {
		return k, true, k > 0
	}
	more, ok := p.mixBlendNoLock(dst[k:], p.currentOutput())
	return k + more, ok, true
}

func (a *automixState) feedMeter(samples [][2]float64, rate beep.SampleRate) {
	if a.meter == nil || a.meterAt != rate {
		a.meter = automix.NewLevelMeter(float64(rate))
		a.meterAt = rate
	}
	a.meter.Feed(samples)
}

func (p *beepPlayer) currentOutput() beep.Streamer {
	if p.gaplessOutput != nil {
		return p.gaplessOutput
	}
	return p.curStreamer
}

func (p *beepPlayer) mixBlendNoLock(dst [][2]float64, incoming beep.Streamer) (int, bool) {
	a := p.automix
	b := a.blend
	a.inBuf = grow(a.inBuf, len(dst))
	a.outBuf = grow(a.outBuf, len(dst))
	nIn, okIn := incoming.Stream(a.inBuf)
	var out [][2]float64
	if !b.tailEnded && !b.mixer.OutgoingDone() {
		nOut, okOut := b.tail.Stream(a.outBuf)
		out = a.outBuf[:nOut]
		if !okOut || nOut < len(dst) {
			b.tailEnded = true
		}
	}
	n := len(dst)
	if !okIn && nIn < n {
		n = nIn
	}
	b.mixer.Process(dst[:n], out, a.inBuf[:nIn])
	if b.mixer.Done() || (!okIn && nIn == 0) {
		b.release()
		a.blend = nil
		a.plan = nil
	}
	return n, okIn || n > 0
}

// startBlendNoLock 到点：取出预备好的下一首，按实时读数定下形状，交换当前曲目并开始混合。
func (p *beepPlayer) startBlendNoLock(tail beep.Streamer, outputRate beep.SampleRate) bool {
	a := p.automix
	plan := *a.plan
	a.plan = nil
	fromID := p.curMusic.Id
	pos, length := p.positionNoLock()

	fromProfile, _ := a.profile(fromID)
	toProfile, _ := a.profile(a.planTo)
	if a.settings.Mode != automix.ModeAutomix {
		fromProfile, toProfile = nil, nil
	}
	sounding, _ := automix.EndsOf(automix.TransitionTrack{Duration: length, Profile: fromProfile})
	remaining := sounding - pos
	overlap := automix.ResolveOverlap(plan, remaining)
	if overlap <= 0 {
		slog.Info("automix dropping blend, the outgoing track ran out first", "remaining", remaining)
		return false
	}
	prepared := p.gapless.takeIfReady(fromID)
	if prepared == nil || prepared.music.Id != a.planTo {
		if prepared != nil {
			prepared.close()
		}
		slog.Info("automix dropping blend, the next track is no longer the planned one")
		return false
	}

	var periodSec, nextBeatIn *float64
	if fromProfile != nil {
		if bpm := automix.SettledBPM(fromProfile.BPM, fromProfile.OutroBPM); bpm != nil {
			periodSec = floatPtr(60 / *bpm)
		}
		steady := fromProfile.BPM != nil && (fromProfile.OutroBPM == nil || math.Abs(*fromProfile.OutroBPM-*fromProfile.BPM) < 0.5)
		if steady {
			period := 60 / *fromProfile.BPM
			offset := math.Mod(fromProfile.BeatOffset, period)
			next := offset + math.Ceil((pos-offset)/period)*period
			nextBeatIn = floatPtr(next - pos)
		}
	}
	var outgoingDB *float64
	if a.meter != nil {
		outgoingDB = a.meter.DB()
	}
	fade := automix.PlanBlendShape(automix.BlendShapeInput{
		Overlap: overlap, OutgoingDB: outgoingDB, NextBeatIn: nextBeatIn, PeriodSec: periodSec,
		MinOverlap: plan.MinOverlap, MaxOverlap: remaining,
	})
	var leadIn *float64
	if toProfile != nil {
		leadIn = floatPtr(toProfile.LeadIn)
	}
	shape := automix.ShapeBlend(automix.BlendShapeRequest{
		Style: plan.Style, Room: overlap, Overlap: fade.Overlap, Crossover: fade.Crossover,
		NextBeatIn: nextBeatIn, PeriodSec: periodSec, IncomingLeadIn: leadIn,
	})
	trimDB := 0.0
	if shape.Overlap >= automix.MinOverlapSec && fromProfile != nil && toProfile != nil {
		trimDB = automix.TrimForBalance(fromProfile.Loudness, toProfile.Loudness)
	}
	gesture, noStems := p.planStemGestureNoLock(plan, shape, pos, fromID, a.planTo, fromProfile, toProfile, outputRate)
	mixer := automix.NewMixer(automix.MixerConfig{
		SampleRate: float64(outputRate), Blend: shape, Plan: plan, TrimDB: trimDB, PeriodSec: periodSec,
		Stems: gesture,
	})

	var log strings.Builder
	fmt.Fprintf(&log, "%s", shape.Style)
	if shape.Hold > 0.01 {
		fmt.Fprintf(&log, ": waits %.2fs, then", shape.Hold)
	} else {
		log.WriteString(":")
	}
	if shape.Overlap < 0.1 {
		fmt.Fprintf(&log, " %dms", int(math.Round(shape.Overlap*1000)))
	} else {
		fmt.Fprintf(&log, " %.2fs", shape.Overlap)
	}
	fmt.Fprintf(&log, " at %d%%", int(math.Round(shape.Crossover*100)))
	if shape.Together > 0 {
		fmt.Fprintf(&log, ", both held for %d%%", int(math.Round(shape.Together*100)))
	}
	switch {
	case gesture != nil:
		log.WriteString(", four stems")
	case noStems != "":
		log.WriteString(", " + noStems)
	}
	if shape.ShapeBands && gesture == nil {
		log.WriteString(", three bands")
	}
	if shape.SweepOut && gesture == nil {
		log.WriteString(", swept out")
	}
	if plan.EchoThrow && gesture == nil {
		log.WriteString(", thrown")
	}
	if plan.Stretch != 1 {
		fmt.Fprintf(&log, ", tempo bend %.3fx not applied", plan.Stretch)
	}
	if fade.SnappedToBeat && periodSec != nil {
		fmt.Fprintf(&log, " on a beat (%d BPM)", int(math.Round(60 / *periodSec)))
	}
	if outgoingDB != nil {
		fmt.Fprintf(&log, ", outgoing %.1f dB", *outgoingDB)
	}
	if trimDB > 0.05 {
		fmt.Fprintf(&log, ", outgoing trimmed %.1f dB", trimDB)
	}
	slog.Info("automix blend", "from", p.curMusic.Name, "to", prepared.music.Name, "detail", log.String())
	if gesture != nil {
		slog.Info("automix stems", "detail", describeStemGesture(gesture))
	}

	release := p.swapToPreparedNoLock(prepared, true)
	a.blend = &activeBlend{tail: tail, release: release, mixer: mixer}
	return true
}

func floatPtr(v float64) *float64 { return &v }
