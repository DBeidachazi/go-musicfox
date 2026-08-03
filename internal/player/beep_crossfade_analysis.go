package player

import (
	"math"
	"sync"

	"github.com/gopxl/beep"
)

const (
	transitionAnalysisRate = 20
	transitionFFTSize      = 1024
)

type transitionFrame struct {
	energy float64
	flux   float64
	onset  float64
}

type transitionSampleHistory struct {
	mu      sync.Mutex
	samples [][2]float64
	limit   int
}

func (h *transitionSampleHistory) setLimit(limit int) {
	h.mu.Lock()
	h.limit = max(0, limit)
	if len(h.samples) > h.limit {
		h.samples = append([][2]float64(nil), h.samples[len(h.samples)-h.limit:]...)
	}
	h.mu.Unlock()
}

func (h *transitionSampleHistory) append(samples [][2]float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.limit <= 0 || len(samples) == 0 {
		return
	}
	if len(samples) >= h.limit {
		h.samples = append(h.samples[:0], samples[len(samples)-h.limit:]...)
		return
	}
	overflow := len(h.samples) + len(samples) - h.limit
	if overflow > 0 {
		copy(h.samples, h.samples[overflow:])
		h.samples = h.samples[:len(h.samples)-overflow]
	}
	h.samples = append(h.samples, samples...)
}

func (h *transitionSampleHistory) snapshot(limit int) [][2]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := min(max(0, limit), len(h.samples))
	return append([][2]float64(nil), h.samples[len(h.samples)-count:]...)
}

func (h *transitionSampleHistory) reset() {
	h.mu.Lock()
	h.samples = h.samples[:0]
	h.mu.Unlock()
}

type transitionHistoryStreamer struct {
	streamer beep.Streamer
	history  *transitionSampleHistory
	onStream func([][2]float64)
}

func (s *transitionHistoryStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	n, ok = s.streamer.Stream(samples)
	if n > 0 {
		s.history.append(samples[:n])
		if s.onStream != nil {
			s.onStream(samples[:n])
		}
	}
	return n, ok
}

func (s *transitionHistoryStreamer) Err() error {
	return s.streamer.Err()
}

func readStreamerPrefix(streamer beep.Streamer, limit int) ([][2]float64, beep.Streamer) {
	if streamer == nil || limit <= 0 {
		return nil, streamer
	}

	prefix := make([][2]float64, 0, limit)
	buffer := make([][2]float64, min(2048, limit))
	for len(prefix) < limit {
		want := min(len(buffer), limit-len(prefix))
		n, ok := streamer.Stream(buffer[:want])
		if n > 0 {
			prefix = append(prefix, buffer[:n]...)
		}
		if !ok || n == 0 {
			break
		}
	}
	return prefix, streamer
}

func newSampleSliceStreamer(samples [][2]float64) beep.Streamer {
	position := 0
	return beep.StreamerFunc(func(dst [][2]float64) (n int, ok bool) {
		if position >= len(samples) {
			return 0, false
		}
		n = copy(dst, samples[position:])
		position += n
		return n, position < len(samples)
	})
}

func adaptiveCrossfadeSamples(outgoing, incoming [][2]float64, rate beep.SampleRate, configuredMax int) (fadeSamples, incomingStart int) {
	availableMax := min(configuredMax, len(outgoing))
	if rate <= 0 || availableMax <= 0 || len(incoming) == 0 {
		return max(0, availableMax), 0
	}

	incomingFrames, frameSamples := analyzeTransitionSamples(incoming, rate)
	outgoingFrames, _ := analyzeTransitionSamples(outgoing, rate)
	if frameSamples <= 0 || len(incomingFrames) < 4 || len(outgoingFrames) < 4 {
		return min(availableMax, len(incoming)), 0
	}

	maxSkipFrames := min(len(incomingFrames)/2, transitionAnalysisRate)
	for incomingStart/frameSamples < maxSkipFrames {
		frame := incomingFrames[incomingStart/frameSamples]
		if frame.energy >= 0.08 || frame.flux >= 0.10 {
			break
		}
		incomingStart += frameSamples
	}
	availableMax = min(availableMax, len(incoming)-incomingStart)
	if availableMax <= 0 {
		return min(configuredMax, min(len(outgoing), len(incoming))), 0
	}

	maxFrames := min(availableMax/frameSamples, min(len(outgoingFrames), len(incomingFrames)-incomingStart/frameSamples))
	if maxFrames < 4 {
		return availableMax, incomingStart
	}
	minFrames := min(maxFrames, max(4, transitionAnalysisRate*3/2))
	startFrame := incomingStart / frameSamples

	for frames := maxFrames; frames >= minFrames; frames-- {
		outStart := len(outgoingFrames) - frames
		conflict := 0.0
		for i := 0; i < frames; i++ {
			out := outgoingFrames[outStart+i]
			in := incomingFrames[startFrame+i]
			conflict += out.energy*in.energy + 0.6*out.onset*in.onset
		}
		if conflict/float64(frames) <= 0.24 {
			return min(availableMax, frames*frameSamples), incomingStart
		}
	}
	return min(availableMax, minFrames*frameSamples), incomingStart
}

func analyzeTransitionSamples(samples [][2]float64, rate beep.SampleRate) ([]transitionFrame, int) {
	frameSamples := max(1, int(rate)/transitionAnalysisRate)
	frameCount := len(samples) / frameSamples
	if frameCount == 0 {
		return nil, frameSamples
	}

	frames := make([]transitionFrame, frameCount)
	var previous [transitionFFTSize / 2]float64
	fluxes := make([]float64, frameCount)
	for frameIndex := 0; frameIndex < frameCount; frameIndex++ {
		start := frameIndex * frameSamples
		frame := samples[start : start+frameSamples]
		rms := frameRMS(frame)
		frames[frameIndex].energy = normalizeTransitionEnergy(rms)

		magnitudes := transitionMagnitudes(frame)
		flux := 0.0
		total := 0.0
		for i, magnitude := range magnitudes {
			if magnitude > previous[i] {
				flux += magnitude - previous[i]
			}
			total += magnitude
			previous[i] = magnitude
		}
		if total > 0 {
			flux /= total
		}
		frames[frameIndex].flux = min(1, flux)
		fluxes[frameIndex] = flux
	}

	medianFlux := transitionMedian(fluxes)
	onsetThreshold := max(0.08, medianFlux*2.5)
	for i := range frames {
		if frames[i].energy < 0.05 || frames[i].flux <= onsetThreshold {
			continue
		}
		frames[i].onset = min(1, frames[i].flux/onsetThreshold-1)
	}
	return frames, frameSamples
}

func frameRMS(samples [][2]float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sum := 0.0
	for _, sample := range samples {
		mono := (sample[0] + sample[1]) * 0.5
		sum += mono * mono
	}
	return math.Sqrt(sum / float64(len(samples)))
}

func normalizeTransitionEnergy(rms float64) float64 {
	if rms <= 0 {
		return 0
	}
	db := 20 * math.Log10(rms)
	return min(1, max(0, (db+60)/54))
}

func transitionMagnitudes(samples [][2]float64) [transitionFFTSize / 2]float64 {
	var fft [transitionFFTSize]complex128
	count := min(len(samples), transitionFFTSize)
	for i := 0; i < count; i++ {
		mono := (samples[i][0] + samples[i][1]) * 0.5
		window := 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(max(1, count-1))))
		fft[i] = complex(mono*window, 0)
	}

	for i, j := 1, 0; i < transitionFFTSize; i++ {
		bit := transitionFFTSize >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			fft[i], fft[j] = fft[j], fft[i]
		}
	}
	for size := 2; size <= transitionFFTSize; size <<= 1 {
		angle := -2 * math.Pi / float64(size)
		step := complex(math.Cos(angle), math.Sin(angle))
		for offset := 0; offset < transitionFFTSize; offset += size {
			weight := complex(1, 0)
			half := size / 2
			for i := 0; i < half; i++ {
				even := fft[offset+i]
				odd := weight * fft[offset+i+half]
				fft[offset+i] = even + odd
				fft[offset+i+half] = even - odd
				weight *= step
			}
		}
	}

	var magnitudes [transitionFFTSize / 2]float64
	for i := range magnitudes {
		magnitudes[i] = math.Hypot(real(fft[i]), imag(fft[i]))
	}
	return magnitudes
}

func transitionMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	for i := 1; i < len(copyValues); i++ {
		value := copyValues[i]
		j := i - 1
		for ; j >= 0 && copyValues[j] > value; j-- {
			copyValues[j+1] = copyValues[j]
		}
		copyValues[j+1] = value
	}
	return copyValues[len(copyValues)/2]
}
