package player

import (
	"testing"
	"time"

	"github.com/gopxl/beep"
)

func TestNewCrossfadeStreamerMixesForFixedSampleCount(t *testing.T) {
	current := newTestStereoStreamer(4, 1, 0)
	next := newTestStereoStreamer(6, 0, 1)
	fadeDone := false
	done := false
	streamer := newCrossfadeStreamer(current, next, 4, 0, func() {
		fadeDone = true
	}, func() {
		done = true
	})

	samples := drainTestStreamer(t, streamer)
	if got, want := len(samples), 6; got != want {
		t.Fatalf("sample count = %d, want %d", got, want)
	}
	if !fadeDone || !done {
		t.Fatalf("callbacks = fadeDone:%v done:%v, want both true", fadeDone, done)
	}
	for i := 1; i < 4; i++ {
		if samples[i][0] >= samples[i-1][0] {
			t.Fatalf("fade-out is not decreasing at sample %d: %v", i, samples[:4])
		}
		if samples[i][1] <= samples[i-1][1] {
			t.Fatalf("fade-in is not increasing at sample %d: %v", i, samples[:4])
		}
	}
	for i := 4; i < len(samples); i++ {
		if samples[i] != [2]float64{0, 1} {
			t.Fatalf("sample %d = %v, want next track at full gain", i, samples[i])
		}
	}
}

func TestNewCrossfadeStreamerWaitsBeforeMixing(t *testing.T) {
	current := newTestStereoStreamer(6, 1, 0)
	next := newTestStereoStreamer(6, 0, 1)
	streamer := newCrossfadeStreamer(current, next, 3, 3, func() {}, func() {})

	samples := drainTestStreamer(t, streamer)
	if got, want := len(samples), 9; got != want {
		t.Fatalf("sample count = %d, want %d", got, want)
	}
	for i := 0; i < 3; i++ {
		if samples[i] != [2]float64{1, 0} {
			t.Fatalf("sample %d = %v, want outgoing track only", i, samples[i])
		}
	}
}

func TestBeepCrossfadeConfiguration(t *testing.T) {
	p := &beepPlayer{crossfadeDuration: 3 * time.Second}
	if got, want := p.CrossfadeDuration(), 3*time.Second; got != want {
		t.Fatalf("crossfade duration = %s, want %s", got, want)
	}
	p.crossfadePendingID.Store(1)
	if !p.CrossfadePending() {
		t.Fatal("crossfade request is not reported as pending")
	}
	p.crossfadePendingID.Store(0)
	if p.CrossfadePending() {
		t.Fatal("completed crossfade request remains pending")
	}
	if got := p.CrossfadeGeneration(); got != 0 {
		t.Fatalf("crossfade generation = %d, want 0", got)
	}
}

func TestBeepPlayRequestsDirectReplacement(t *testing.T) {
	p := &beepPlayer{
		crossfadeDuration:  3 * time.Second,
		crossfadeMusicChan: make(chan beepCrossfadeRequest, 1),
	}
	p.Play(URLMusic{})
	request := <-p.crossfadeMusicChan
	if request.crossfade {
		t.Fatal("ordinary Play requested a crossfade")
	}
}

func TestBeepPlayCrossfadeRequestsAutomaticTransition(t *testing.T) {
	p := &beepPlayer{
		crossfadeDuration:  3 * time.Second,
		crossfadeMusicChan: make(chan beepCrossfadeRequest, 1),
	}
	p.PlayCrossfade(URLMusic{})
	request := <-p.crossfadeMusicChan
	if !request.crossfade {
		t.Fatal("PlayCrossfade requested a direct replacement")
	}
}

func TestCancelCrossfadeClearsOnlyCurrentPendingRequest(t *testing.T) {
	p := &beepPlayer{crossfadeDuration: 3 * time.Second}
	p.crossfadePendingID.Store(7)
	p.CancelCrossfade()
	if p.CrossfadePending() {
		t.Fatal("cancelled crossfade remains pending")
	}
	p.crossfadePendingID.Store(8)
	p.finishCrossfadeRequest(beepCrossfadeRequest{id: 7})
	if !p.CrossfadePending() {
		t.Fatal("stale request completion cleared a newer request")
	}
}

func newTestStereoStreamer(count int, left, right float64) beep.Streamer {
	position := 0
	return beep.StreamerFunc(func(samples [][2]float64) (n int, ok bool) {
		remaining := count - position
		if remaining <= 0 {
			return 0, false
		}
		if remaining > len(samples) {
			remaining = len(samples)
		}
		for i := 0; i < remaining; i++ {
			samples[i] = [2]float64{left, right}
		}
		position += remaining
		return remaining, position < count
	})
}

func drainTestStreamer(t *testing.T, streamer beep.Streamer) [][2]float64 {
	t.Helper()
	var result [][2]float64
	buffer := make([][2]float64, 3)
	for i := 0; i < 20; i++ {
		n, ok := streamer.Stream(buffer)
		result = append(result, buffer[:n]...)
		if !ok {
			return result
		}
		if n == 0 {
			t.Fatal("streamer returned no samples without draining")
		}
	}
	t.Fatal("streamer did not drain")
	return nil
}
