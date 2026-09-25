package models

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/go-musicfox/go-musicfox/internal/automix"
)

// 真实模型的端到端测试。需要已下载的模型目录：
//
//	MUSICFOX_AUTOMIX_MODELS=<data>/automix/models go test ./internal/automix/models -run Real -v
//
// 可选 MUSICFOX_AUTOMIX_BACKEND=python|native 指定分离后端。

func realStore(t *testing.T) *Store {
	dir := os.Getenv("MUSICFOX_AUTOMIX_MODELS")
	if dir == "" {
		t.Skip("set MUSICFOX_AUTOMIX_MODELS to a downloaded models directory")
	}
	return NewStore(Options{Dir: dir, Level: LevelFull, StemBackend: os.Getenv("MUSICFOX_AUTOMIX_BACKEND"),
		Python: os.Getenv("MUSICFOX_AUTOMIX_PYTHON")})
}

// clicks 120 BPM、4/4，重拍更响的点击加和弦铺底。
func clicks(seconds float64, rate int) []float32 {
	out := make([]float32, int(seconds*float64(rate)))
	period := 0.5
	for i := range out {
		t := float64(i) / float64(rate)
		beat := int(t / period)
		since := t - float64(beat)*period
		amp := 0.5
		if beat%4 == 0 {
			amp = 1
		}
		v := 0.0
		if since < 0.03 {
			v = amp * math.Exp(-since*120) * math.Sin(2*math.Pi*80*since)
		}
		v += 0.05 * (math.Sin(2*math.Pi*220*t) + math.Sin(2*math.Pi*277*t))
		out[i] = float32(v)
	}
	return out
}

func TestRealBeatThis(t *testing.T) {
	s := realStore(t)
	if _, ok := s.BeatThisModel(); !ok {
		t.Skip("beat_this not installed: " + s.Status())
	}
	e := NewEngine(s)
	started := time.Now()
	grid, err := e.BeatGrid(clicks(40, automix.BeatThisSampleRate))
	if err != nil || grid == nil {
		t.Fatalf("grid=%v err=%v", grid, err)
	}
	fields := automix.GridFromBeats(*grid, false)
	t.Logf("%d beats, %d downbeats in %v, fields %+v", len(grid.Beats), len(grid.Downbeats), time.Since(started), fields)
	if fields == nil || math.Abs(*fields.BPM-120) > 2 {
		t.Errorf("expected ~120 BPM")
	}
}

func TestRealHtdemucs(t *testing.T) {
	s := realStore(t)
	e := NewEngine(s)
	if !e.StemsReady() {
		t.Skip("htdemucs not runnable: " + s.Status())
	}
	mono := clicks(10, automix.StemSampleRate)
	started := time.Now()
	w, err := e.SeparateWindow(context.Background(), mono, mono, 100, automix.RoleTail)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("separated %.1fs in %v", w.Duration(), time.Since(started))
	var drums, bass, vocals, other float64
	for i := 0; i < w.Len(); i++ {
		drums += math.Abs(w.Sample(automix.StemDrums, i)[0])
		bass += math.Abs(w.Sample(automix.StemBass, i)[0])
		vocals += math.Abs(w.Sample(automix.StemVocals, i)[0])
		other += math.Abs(w.Sample(automix.StemOther, i)[0])
	}
	if drums == 0 || bass == 0 || vocals == 0 || other == 0 {
		t.Errorf("empty stems: drums %v bass %v vocals %v other %v", drums, bass, vocals, other)
	}
}
