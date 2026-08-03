package player

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopxl/beep"
	"github.com/gopxl/beep/effects"
	"github.com/gopxl/beep/speaker"

	"github.com/go-musicfox/go-musicfox/internal/configs"
	"github.com/go-musicfox/go-musicfox/internal/types"
	"github.com/go-musicfox/go-musicfox/utils/app"
	"github.com/go-musicfox/go-musicfox/utils/errorx"
	"github.com/go-musicfox/go-musicfox/utils/iox"
	"github.com/go-musicfox/go-musicfox/utils/timex"
)

type beepCrossfadeTrack struct {
	mu sync.Mutex

	music           URLMusic
	format          beep.Format
	streamer        beep.StreamSeekCloser
	cacheReader     *os.File
	cacheWriter     *os.File
	cachePath       string
	cacheDownloaded bool
	consumer        func(sampleRate float64, samplesL, samplesR []float32)
	transitionTail  transitionSampleHistory
	playedSamples   atomic.Int64
	cancel          context.CancelFunc
	closed          chan struct{}
	closeOnce       sync.Once
}

type beepCrossfadeRequest struct {
	music      URLMusic
	generation int64
	crossfade  bool
	id         int64
}

func (p *beepPlayer) listenCrossfade() {
	if err := speaker.Init(sampleRate, sampleRate.N(time.Millisecond*200)); err != nil {
		panic(err)
	}

	p.l.Lock()
	p.crossfadeMixer = &beep.Mixer{}
	p.ctrl.Streamer = p.crossfadeMixer
	p.volume.Streamer = p.ctrl
	p.l.Unlock()
	speaker.Play(p.volume)

	done := make(chan *beepCrossfadeTrack, 10)
	for {
		select {
		case <-p.close:
			return
		case track := <-done:
			p.l.Lock()
			isCurrent := p.crossfadeCurrent == track
			if isCurrent {
				p.stopCrossfadeNoLock()
			}
			p.l.Unlock()
		case request := <-p.crossfadeMusicChan:
			p.playCrossfadeTrack(request, done)
		}
	}
}

func (p *beepPlayer) playCrossfadeTrack(request beepCrossfadeRequest, done chan<- *beepCrossfadeTrack) {
	if request.generation != p.crossfadeGeneration.Load() {
		p.finishCrossfadeRequest(request)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.l.Lock()
	p.crossfadePrepareCancel = cancel
	p.l.Unlock()
	track, err := p.prepareCrossfadeTrack(ctx, cancel, request.music)
	p.l.Lock()
	p.crossfadePrepareCancel = nil
	p.l.Unlock()
	if err != nil {
		p.finishCrossfadeRequest(request)
		slog.Error("prepare crossfade track failed", "error", err, "song", request.music.Name)
		if request.generation != p.crossfadeGeneration.Load() {
			return
		}
		p.l.Lock()
		if p.state == types.Stopped {
			p.setState(types.Stopped)
		} else {
			p.stopCrossfadeNoLock()
		}
		p.l.Unlock()
		return
	}
	if request.generation != p.crossfadeGeneration.Load() {
		track.Close()
		p.finishCrossfadeRequest(request)
		return
	}
	select {
	case <-p.close:
		track.Close()
		p.finishCrossfadeRequest(request)
		return
	default:
	}
	nextStream := track.output()
	var nextPrefix [][2]float64
	if request.crossfade {
		nextPrefix, nextStream = readStreamerPrefix(nextStream, sampleRate.N(p.crossfadeDuration))
	}
	p.l.Lock()
	if request.generation != p.crossfadeGeneration.Load() {
		p.l.Unlock()
		track.Close()
		p.finishCrossfadeRequest(request)
		return
	}
	oldTrack := p.crossfadeCurrent
	oldStream := p.crossfadeCurrentStream
	staleTracks := append([]*beepCrossfadeTrack(nil), p.crossfadeOutgoing...)
	canCrossfade := request.crossfade && p.state == types.Playing && oldTrack != nil && oldStream != nil && p.crossfadePlayback != nil
	configuredSamples := sampleRate.N(p.crossfadeDuration)
	if canCrossfade {
		remaining := oldTrack.music.Duration - oldTrack.PassedTime()
		if remaining <= 0 {
			canCrossfade = false
		} else {
			configuredSamples = min(configuredSamples, sampleRate.N(remaining))
		}
	}
	var oldTail [][2]float64
	if oldTrack != nil {
		oldTail = oldTrack.transitionTail.snapshot(configuredSamples)
	}
	fadeSamples, nextStart := adaptiveCrossfadeSamples(oldTail, nextPrefix, sampleRate, configuredSamples)
	if nextStart > len(nextPrefix) {
		nextStart = len(nextPrefix)
	}

	speaker.Lock()
	canCrossfade = canCrossfade && p.crossfadeMixer.Len() > 0
	if canCrossfade {
		slog.Debug("selected adaptive crossfade",
			"duration", sampleRate.D(fadeSamples),
			"maximum", sampleRate.D(configuredSamples),
			"incoming_skip", sampleRate.D(nextStart),
			"song", request.music.Name,
		)
		oldTrack.setConsumer(nil)
		nextStream = beep.Seq(newSampleSliceStreamer(nextPrefix[nextStart:]), nextStream)
		nextStream = track.playbackStream(nextStream, sampleRate.N(p.crossfadeDuration), nextStart)
		waitSamples := configuredSamples - fadeSamples
		if waitSamples < 0 {
			waitSamples = 0
		}
		p.crossfadePlayback.Streamer = newCrossfadeStreamer(
			oldStream,
			nextStream,
			fadeSamples,
			waitSamples,
			oldTrack.Close,
			func() { notifyCrossfadeDone(p.close, done, track) },
		)
		p.crossfadeOutgoing = []*beepCrossfadeTrack{oldTrack}
	} else {
		nextStream = beep.Seq(newSampleSliceStreamer(nextPrefix), nextStream)
		nextStream = track.playbackStream(nextStream, sampleRate.N(p.crossfadeDuration), 0)
		if oldTrack != nil {
			staleTracks = append(staleTracks, oldTrack)
		}
		p.crossfadeMixer.Clear()
		p.crossfadePlayback = &beep.Ctrl{Streamer: beep.Seq(
			nextStream,
			beep.Callback(func() { notifyCrossfadeDone(p.close, done, track) }),
		)}
		p.crossfadeMixer.Add(p.crossfadePlayback)
		p.crossfadeOutgoing = nil
	}
	p.crossfadeCurrent = track
	p.crossfadeCurrentStream = nextStream
	if p.spectrum != nil {
		track.consumer = p.spectrum.NewConsumer()
	}
	p.curMusic = request.music
	p.ctrl.Paused = false
	speaker.Unlock()

	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = p.newCrossfadeTimer(track)
	go p.timer.Run()
	p.setState(types.Playing)
	p.l.Unlock()
	p.finishCrossfadeRequest(request)

	for _, stale := range staleTracks {
		if stale != oldTrack || !canCrossfade {
			stale.Close()
		}
	}
}

func (p *beepPlayer) finishCrossfadeRequest(request beepCrossfadeRequest) {
	p.crossfadePendingID.CompareAndSwap(request.id, 0)
}

func newCrossfadeStreamer(current, next beep.Streamer, fadeSamples, waitSamples int, onFadeDone, onDone func()) beep.Streamer {
	if fadeSamples <= 0 {
		return beep.Seq(
			beep.Callback(onFadeDone),
			next,
			beep.Callback(onDone),
		)
	}
	return beep.Seq(
		beep.Take(waitSamples, current),
		beep.Mix(
			effects.Transition(
				beep.Take(fadeSamples, current),
				fadeSamples,
				1,
				0,
				effects.TransitionEqualPower,
			),
			effects.Transition(
				beep.Take(fadeSamples, next),
				fadeSamples,
				0,
				1,
				effects.TransitionEqualPower,
			),
		),
		beep.Callback(onFadeDone),
		next,
		beep.Callback(onDone),
	)
}

func notifyCrossfadeDone(closed <-chan struct{}, done chan<- *beepCrossfadeTrack, track *beepCrossfadeTrack) {
	select {
	case done <- track:
	case <-closed:
	default:
	}
}

func (p *beepPlayer) newCrossfadeTimer(track *beepCrossfadeTrack) *timex.Timer {
	return timex.NewTimer(timex.Options{
		Duration:       8760 * time.Hour,
		TickerInternal: configs.AppConfig.Main.FrameRate.Interval(),
		OnRun:          func(started bool) {},
		OnPause:        func() {},
		OnDone:         func(stopped bool) {},
		OnTick: func() {
			select {
			case p.timeChan <- track.PassedTime():
			default:
			}
		},
	})
}

func (p *beepPlayer) prepareCrossfadeTrack(ctx context.Context, cancel context.CancelFunc, music URLMusic) (*beepCrossfadeTrack, error) {
	track := &beepCrossfadeTrack{
		music:  music,
		cancel: cancel,
		closed: make(chan struct{}),
	}
	if strings.HasPrefix(music.URL, "file://") {
		reader, err := os.Open(strings.TrimPrefix(music.URL, "file://"))
		if err != nil {
			return nil, err
		}
		track.cacheReader = reader
		track.cacheDownloaded = true
		track.streamer, track.format, err = DecodeSong(music.Type, reader)
		if err != nil {
			track.Close()
			return nil, err
		}
		return track, nil
	}

	writer, err := os.CreateTemp(app.RuntimeDir(), "beep_crossfade_*")
	if err != nil {
		return nil, err
	}
	track.cacheWriter = writer
	track.cachePath = writer.Name()
	reader, err := os.Open(track.cachePath)
	if err != nil {
		track.Close()
		return nil, err
	}
	track.cacheReader = reader

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, music.URL, nil)
	if err != nil {
		track.Close()
		return nil, err
	}
	response, err := p.httpClient.Do(request)
	if err != nil {
		track.Close()
		return nil, err
	}
	go p.downloadCrossfadeTrack(ctx, track, writer, response.Body)

	waitBytes := 512
	if music.Type == Flac {
		waitBytes *= 4
	}
	if err = iox.WaitForNBytes(reader, waitBytes, 100*time.Millisecond, 50); err != nil {
		track.Close()
		return nil, err
	}

	track.mu.Lock()
	track.streamer, track.format, err = DecodeSong(music.Type, reader)
	track.mu.Unlock()
	if err != nil {
		track.Close()
		return nil, err
	}
	return track, nil
}

func (p *beepPlayer) downloadCrossfadeTrack(ctx context.Context, track *beepCrossfadeTrack, writer *os.File, reader io.ReadCloser) {
	_, copyErr := iox.CopyClose(ctx, writer, reader)
	_ = writer.Close()

	track.mu.Lock()
	defer track.mu.Unlock()
	if track.cacheWriter == writer {
		track.cacheWriter = nil
	}
	select {
	case <-track.closed:
		return
	default:
	}
	track.cacheDownloaded = true
	if copyErr != nil {
		slog.Warn("crossfade track download stopped", "error", copyErr, "song", track.music.Name)
		return
	}
	if track.music.Type != Mp3 || configs.AppConfig.Player.Beep.Mp3Decoder == types.BeepMiniMp3Decoder || track.streamer == nil {
		return
	}

	cacheReader, err := os.Open(track.cachePath)
	if err != nil {
		return
	}
	streamer, format, err := DecodeSong(track.music.Type, cacheReader)
	if err != nil {
		_ = cacheReader.Close()
		return
	}
	position := track.streamer.Position()
	if position >= streamer.Len() {
		position = streamer.Len() - 1
	}
	if position < 0 {
		position = 0
	}
	if err = streamer.Seek(position); err != nil {
		_ = streamer.Close()
		return
	}
	oldStreamer := track.streamer
	track.streamer = streamer
	track.format = format
	track.cacheReader = cacheReader
	_ = oldStreamer.Close()
}

func (t *beepCrossfadeTrack) output() beep.Streamer {
	t.mu.Lock()
	trackSampleRate := t.format.SampleRate
	t.mu.Unlock()
	var output beep.Streamer = t
	if trackSampleRate == sampleRate {
		output = t
	} else {
		output = beep.Resample(resampleQuiality, trackSampleRate, sampleRate, t)
	}
	return output
}

func (t *beepCrossfadeTrack) playbackStream(streamer beep.Streamer, historySamples, initialSamples int) beep.Streamer {
	t.transitionTail.setLimit(historySamples)
	t.playedSamples.Store(int64(initialSamples))
	return &transitionHistoryStreamer{
		streamer: streamer,
		history:  &t.transitionTail,
		onStream: func(samples [][2]float64) {
			t.playedSamples.Add(int64(len(samples)))
			t.consumePlaybackSamples(samples)
		},
	}
}

func (t *beepCrossfadeTrack) Stream(samples [][2]float64) (n int, ok bool) {
	for retry := 4; ; retry-- {
		t.mu.Lock()
		select {
		case <-t.closed:
			t.mu.Unlock()
			return 0, false
		default:
		}
		if t.streamer == nil {
			t.mu.Unlock()
			return 0, false
		}

		position := t.streamer.Position()
		n, ok = t.streamer.Stream(samples)
		err := t.streamer.Err()
		if err == nil && (ok || t.cacheDownloaded) {
			t.mu.Unlock()
			return n, ok
		}
		if retry == 0 {
			t.mu.Unlock()
			return n, ok
		}
		if t.music.Type == Flac {
			if err = t.streamer.Seek(position); err != nil {
				t.mu.Unlock()
				return n, ok
			}
		}
		errorx.ResetError(t.streamer)
		t.mu.Unlock()

		select {
		case <-time.After(5 * time.Second):
		case <-t.closed:
			return 0, false
		}
	}
}

func (t *beepCrossfadeTrack) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streamer == nil {
		return nil
	}
	return t.streamer.Err()
}

func (t *beepCrossfadeTrack) PassedTime() time.Duration {
	return sampleRate.D(int(t.playedSamples.Load()))
}

func (t *beepCrossfadeTrack) Seek(duration time.Duration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streamer == nil || !t.cacheDownloaded {
		return nil
	}
	position := t.format.SampleRate.N(duration)
	if position < 0 {
		position = 0
	}
	if position >= t.streamer.Len() {
		position = t.streamer.Len() - 1
	}
	if position < 0 {
		return nil
	}
	if err := t.streamer.Seek(position); err != nil {
		return err
	}
	t.transitionTail.reset()
	t.playedSamples.Store(int64(sampleRate.N(duration)))
	return nil
}

func (t *beepCrossfadeTrack) setConsumer(consumer func(sampleRate float64, samplesL, samplesR []float32)) {
	t.mu.Lock()
	t.consumer = consumer
	t.mu.Unlock()
}

func (t *beepCrossfadeTrack) consumePlaybackSamples(samples [][2]float64) {
	t.mu.Lock()
	consumer := t.consumer
	t.mu.Unlock()
	if consumer == nil || len(samples) == 0 {
		return
	}
	samplesL := make([]float32, len(samples))
	samplesR := make([]float32, len(samples))
	for i, sample := range samples {
		samplesL[i] = float32(sample[0])
		samplesR[i] = float32(sample[1])
	}
	consumer(float64(sampleRate), samplesL, samplesR)
}

func (t *beepCrossfadeTrack) Close() {
	t.closeOnce.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
		close(t.closed)

		t.mu.Lock()
		if t.streamer != nil {
			_ = t.streamer.Close()
			t.streamer = nil
		}
		if t.cacheReader != nil {
			_ = t.cacheReader.Close()
			t.cacheReader = nil
		}
		if t.cacheWriter != nil {
			_ = t.cacheWriter.Close()
			t.cacheWriter = nil
		}
		cachePath := t.cachePath
		t.mu.Unlock()

		if cachePath != "" {
			_ = os.Remove(cachePath)
		}
	})
}

func (p *beepPlayer) seekCrossfade(duration time.Duration) {
	if duration < 0 || configs.AppConfig.Player.Beep.Mp3Decoder == types.BeepMiniMp3Decoder {
		return
	}
	p.l.Lock()
	track := p.crossfadeCurrent
	state := p.state
	timer := p.timer
	p.l.Unlock()
	if track == nil || track.music.Type != Mp3 || (state != types.Playing && state != types.Paused) {
		return
	}

	speaker.Lock()
	err := track.Seek(duration)
	speaker.Unlock()
	if err != nil {
		slog.Error("seek error", "error", err)
		return
	}
	if timer != nil {
		timer.SetPassed(duration)
	}
}

func (p *beepPlayer) stopCrossfadeNoLock() {
	speaker.Lock()
	p.ctrl.Paused = true
	if p.crossfadeMixer != nil {
		p.crossfadeMixer.Clear()
	}
	speaker.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.closeCrossfadeTracksNoLock()
	p.setState(types.Stopped)
}

func (p *beepPlayer) closeCrossfadeNoLock() {
	if p.crossfadeClosed {
		return
	}
	p.crossfadeClosed = true
	p.crossfadeGeneration.Add(1)
	if p.crossfadePrepareCancel != nil {
		p.crossfadePrepareCancel()
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	speaker.Lock()
	p.ctrl.Paused = true
	if p.crossfadeMixer != nil {
		p.crossfadeMixer.Clear()
	}
	speaker.Unlock()
	p.closeCrossfadeTracksNoLock()
	if p.close != nil {
		close(p.close)
	}
	if p.spectrum != nil {
		p.spectrum.Close()
	}
	speaker.Clear()
	speaker.Close()
}

func (p *beepPlayer) closeCrossfadeTracksNoLock() {
	tracks := append([]*beepCrossfadeTrack(nil), p.crossfadeOutgoing...)
	if p.crossfadeCurrent != nil {
		tracks = append(tracks, p.crossfadeCurrent)
	}
	p.crossfadeOutgoing = nil
	p.crossfadeCurrent = nil
	p.crossfadeCurrentStream = nil
	p.crossfadePlayback = nil
	for _, track := range tracks {
		track.Close()
	}
}
