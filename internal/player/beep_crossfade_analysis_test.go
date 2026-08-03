package player

import (
	"math"
	"testing"
	"time"

	"github.com/gopxl/beep"
)

func TestBufferStreamerPrefixPreservesSamples(t *testing.T) {
	original := makeTestTransitionSamples(20, func(i int) float64 { return float64(i) / 20 })
	prefix, remainder := readStreamerPrefix(newSampleSliceStreamer(original), 7)
	if got, want := len(prefix), 7; got != want {
		t.Fatalf("prefix length = %d, want %d", got, want)
	}
	replayed := drainTestStreamer(t, beep.Seq(newSampleSliceStreamer(prefix), remainder))
	if len(replayed) != len(original) {
		t.Fatalf("replayed length = %d, want %d", len(replayed), len(original))
	}
	for i := range original {
		if replayed[i] != original[i] {
			t.Fatalf("sample %d = %v, want %v", i, replayed[i], original[i])
		}
	}
}

func TestTransitionSampleHistoryKeepsNewestSamples(t *testing.T) {
	var history transitionSampleHistory
	history.setLimit(4)
	history.append(makeTestTransitionSamples(3, func(i int) float64 { return float64(i + 1) }))
	history.append(makeTestTransitionSamples(3, func(i int) float64 { return float64(i + 4) }))

	samples := history.snapshot(10)
	if len(samples) != 4 {
		t.Fatalf("history length = %d, want 4", len(samples))
	}
	for i, want := range []float64{3, 4, 5, 6} {
		if samples[i][0] != want {
			t.Fatalf("history sample %d = %v, want %v", i, samples[i][0], want)
		}
	}
	history.reset()
	if got := len(history.snapshot(10)); got != 0 {
		t.Fatalf("history length after reset = %d, want 0", got)
	}
}

func TestPlaybackStreamCountsOnlyRenderedSamples(t *testing.T) {
	track := &beepCrossfadeTrack{}
	streamer := track.playbackStream(newSampleSliceStreamer(makeTestTransitionSamples(20, func(int) float64 { return 0.2 })), 10, 4)
	buffer := make([][2]float64, 6)
	n, _ := streamer.Stream(buffer)
	if n != 6 {
		t.Fatalf("streamed samples = %d, want 6", n)
	}
	if got := track.playedSamples.Load(); got != 10 {
		t.Fatalf("played samples = %d, want initial 4 + rendered 6", got)
	}
}

func TestAdaptiveCrossfadeKeepsLongQuietTransition(t *testing.T) {
	rate := beep.SampleRate(1000)
	maxSamples := rate.N(5 * time.Second)
	outgoing := makeTestTransitionSamples(maxSamples, func(int) float64 { return 0.002 })
	incoming := makeTestTransitionSamples(maxSamples, func(int) float64 { return 0.002 })

	fade, start := adaptiveCrossfadeSamples(outgoing, incoming, rate, maxSamples)
	if fade != maxSamples {
		t.Fatalf("fade samples = %d, want configured maximum %d", fade, maxSamples)
	}
	if start != 0 {
		t.Fatalf("incoming start = %d, want 0", start)
	}
}

func TestAdaptiveCrossfadeShortensDenseOverlap(t *testing.T) {
	rate := beep.SampleRate(1000)
	maxSamples := rate.N(5 * time.Second)
	dense := makeTestTransitionSamples(maxSamples, func(i int) float64 {
		return 0.7 * math.Sin(2*math.Pi*float64(i)/20)
	})

	fade, _ := adaptiveCrossfadeSamples(dense, dense, rate, maxSamples)
	if fade >= maxSamples {
		t.Fatalf("fade samples = %d, want less than %d for dense overlap", fade, maxSamples)
	}
	if fade < rate.N(1500*time.Millisecond) {
		t.Fatalf("fade samples = %d, want at least the minimum transition", fade)
	}
}

func TestAdaptiveCrossfadeSkipsLimitedLeadingSilence(t *testing.T) {
	rate := beep.SampleRate(1000)
	maxSamples := rate.N(5 * time.Second)
	outgoing := makeTestTransitionSamples(maxSamples, func(int) float64 { return 0.01 })
	incoming := makeTestTransitionSamples(maxSamples, func(i int) float64 {
		if i < rate.N(500*time.Millisecond) {
			return 0
		}
		return 0.3
	})

	_, start := adaptiveCrossfadeSamples(outgoing, incoming, rate, maxSamples)
	if start < rate.N(400*time.Millisecond) || start > rate.N(600*time.Millisecond) {
		t.Fatalf("incoming start = %d, want about 500 ms", start)
	}
}

func makeTestTransitionSamples(count int, value func(int) float64) [][2]float64 {
	samples := make([][2]float64, count)
	for i := range samples {
		v := value(i)
		samples[i] = [2]float64{v, v}
	}
	return samples
}
