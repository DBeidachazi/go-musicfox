package automix

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// testdata/beatthis_golden.json 由上游 beatThis.ts 原样生成（node --experimental-strip-types），
// 输入用整数 LCG 构造，在两边逐位相同。

func lcg(seed uint32) func() uint32 {
	s := seed
	return func() uint32 {
		s = s*1664525 + 1013904223
		return s
	}
}

func goldenSignal(n int, seed uint32) []float32 {
	r := lcg(seed)
	out := make([]float32, n)
	for i := range out {
		noise := (float64(r()>>8)/16777216 - 0.5) * 0.25
		click := 0.0
		if i%5512 < 64 {
			click = float64((i%5512)%16)/16 - 0.5
		}
		out[i] = float32(noise + click)
	}
	return out
}

func goldenLogits(n int, seed uint32) []float32 {
	r := lcg(seed)
	out := make([]float32, n)
	for i := range out {
		v := float64((r()>>8)%2001)/100 - 10
		if i%25 == 0 {
			v += 12
		}
		if i%26 == 1 {
			v += 11
		}
		out[i] = float32(v)
	}
	return out
}

type beatGolden struct {
	Mels []struct {
		N, Seed, Frames int
		Data            []float64
	}
	Picks []struct {
		N, Seed int
		Grid    struct{ Beats, Downbeats []float64 }
	}
	Chunks []struct {
		Frames int
		Chunks []struct{ Start, Frames int }
	}
	Agg []struct {
		Frames         int
		Beat, Downbeat []float64
	}
}

func loadBeatGolden(t *testing.T) beatGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/beatthis_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g beatGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBeatThisMelMatchesUpstream(t *testing.T) {
	for _, m := range loadBeatGolden(t).Mels {
		mel := BeatThisMel(goldenSignal(m.N, uint32(m.Seed)))
		if mel.Frames != m.Frames || len(mel.Data) != len(m.Data) {
			t.Fatalf("n=%d: %d frames, upstream %d", m.N, mel.Frames, m.Frames)
		}
		worst := 0.0
		for i, want := range m.Data {
			worst = math.Max(worst, math.Abs(float64(mel.Data[i])-want))
		}
		// float32 存储的舍入，两边 FFT 实现相同。
		if worst > 1e-5 {
			t.Errorf("n=%d: max abs diff %g", m.N, worst)
		}
	}
}

func TestBeatsFromLogitsMatchesUpstream(t *testing.T) {
	for _, p := range loadBeatGolden(t).Picks {
		grid := BeatsFromLogits(goldenLogits(p.N, uint32(p.Seed)), goldenLogits(p.N, uint32(p.Seed+100)), BeatThisFPS)
		if !sameTimes(grid.Beats, p.Grid.Beats) || !sameTimes(grid.Downbeats, p.Grid.Downbeats) {
			t.Errorf("n=%d: got %d/%d beats/downbeats, upstream %d/%d", p.N,
				len(grid.Beats), len(grid.Downbeats), len(p.Grid.Beats), len(p.Grid.Downbeats))
		}
	}
}

func sameTimes(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func TestSplitForModelMatchesUpstream(t *testing.T) {
	for _, c := range loadBeatGolden(t).Chunks {
		got := SplitForModel(MelSpectrogram{Data: make([]float32, c.Frames*btMels), Frames: c.Frames})
		if len(got) != len(c.Chunks) {
			t.Fatalf("%d frames: %d chunks, upstream %d", c.Frames, len(got), len(c.Chunks))
		}
		for i, want := range c.Chunks {
			if got[i].Start != want.Start || got[i].Frames != want.Frames {
				t.Errorf("%d frames chunk %d: %+v, upstream %+v", c.Frames, i, got[i], want)
			}
		}
	}
}

func TestAggregateChunksMatchesUpstream(t *testing.T) {
	for _, a := range loadBeatGolden(t).Agg {
		chunks := SplitForModel(MelSpectrogram{Data: make([]float32, a.Frames*btMels), Frames: a.Frames})
		preds := make([]ChunkPrediction, len(chunks))
		for k, c := range chunks {
			p := ChunkPrediction{Start: c.Start, Frames: c.Frames,
				Beat: make([]float32, c.Frames), Downbeat: make([]float32, c.Frames)}
			for o := range p.Beat {
				p.Beat[o] = float32(k*10000 + o)
				p.Downbeat[o] = -float32(k*10000 + o)
			}
			preds[k] = p
		}
		beat, downbeat := AggregateChunks(preds, a.Frames)
		for i := range beat {
			if float64(beat[i]) != a.Beat[i] || float64(downbeat[i]) != a.Downbeat[i] {
				t.Fatalf("%d frames: frame %d = %v/%v, upstream %v/%v", a.Frames, i, beat[i], downbeat[i], a.Beat[i], a.Downbeat[i])
			}
		}
	}
}

type fakeBeatRunner struct{ calls int }

func (f *fakeBeatRunner) RunBeatThis(chunk []float32, frames int) ([]float32, []float32, error) {
	f.calls++
	beat := make([]float32, frames)
	downbeat := make([]float32, frames)
	for i := range beat {
		beat[i], downbeat[i] = -5, -5
	}
	return beat, downbeat, nil
}

func TestAnalyseBeatGridChunksWholeTrack(t *testing.T) {
	r := &fakeBeatRunner{}
	grid, err := AnalyseBeatGrid(make([]float32, 70*BeatThisSampleRate), r)
	if err != nil || grid == nil {
		t.Fatal(grid, err)
	}
	// 70s = 3501 帧 → 3 块
	if r.calls != 3 || len(grid.Beats) != 0 {
		t.Errorf("calls=%d beats=%d", r.calls, len(grid.Beats))
	}
}
