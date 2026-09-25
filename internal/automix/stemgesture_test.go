package automix

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// testdata/stemgesture_golden.json 由上游 stemGesture.ts 原样生成，输入与下面的构造逐位相同。

func goldenVocalEnv(cells int, seed uint32, kind int) []float32 {
	r := lcg(seed)
	v := make([]float32, cells)
	for i := range v {
		u := float64(r()>>8) / 16777216
		level := 0.2 + 0.1*u
		phrase := i % 60
		switch {
		case kind == 0 && phrase >= 48:
			level = 0.0003 * u
		case kind == 1:
			level = 0.15 + 0.1*u
		case kind == 2 && i >= cells-90 && i < cells-10:
			level = 0.3 + 0.02*u
		case kind == 2 && i >= cells-10:
			level = 0.00001
		case kind == 3 && phrase >= 50:
			level = 0.004 * u
		}
		v[i] = float32(level)
	}
	return v
}

func goldenMixEnv(cells int, seed uint32) []float32 {
	r := lcg(seed)
	v := make([]float32, cells)
	for i := range v {
		v[i] = float32(0.3 + 0.2*(float64(r()>>8)/16777216))
	}
	return v
}

type stemGolden struct {
	Cases []struct {
		K, Cells  int
		Downbeats []float64
		Handover  struct {
			Swap, BassAt, VocalIn, DueAt float64
			Exit                         struct {
				From, To float64
				Kind     string
				LoudDb   float64
				Held     *struct{ From, To, HoldDb float64 }
			}
		}
		Sustain *struct{ From, To, HoldDb float64 }
		Last    *float64
		Sings   bool
		Probes  struct{ OutVocals, OutDrums, InOther, InVocals []float64 }
	}
	Env []float64
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestStemHandoverMatchesUpstream(t *testing.T) {
	raw, err := os.ReadFile("testdata/stemgesture_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g stemGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	type cfg struct {
		wall          float64
		outBar, inBar *float64
		kind          int
		sings         *bool
		clash         bool
	}
	f, yes, no := ptr[float64], ptr(true), ptr(false)
	configs := []cfg{
		{12, f(2), f(2.1), 0, nil, false},
		{16.4, f(1.9), nil, 1, nil, false},
		{14.2, f(2.2), f(2.0), 2, nil, false},
		{14.2, f(2.2), f(2.0), 2, nil, true},
		{6.6, nil, nil, 0, no, false},
		{23.5, f(1.6), f(1.7), 3, yes, false},
		{9.3, f(2.4), f(2.4), 3, no, true},
	}
	if len(g.Cases) != len(configs) {
		t.Fatalf("%d golden cases", len(g.Cases))
	}
	for k, c := range configs {
		want := g.Cases[k]
		cells := int(math.Floor(c.wall / 0.05))
		if cells != want.Cells {
			t.Fatalf("case %d: %d cells, upstream %d", k, cells, want.Cells)
		}
		vocals := goldenVocalEnv(cells, uint32(100+k), c.kind)
		mix := goldenMixEnv(cells, uint32(200+k))
		var downbeats []float64
		if c.outBar != nil {
			for at := 0.37; at < c.wall; at += *c.outBar {
				downbeats = append(downbeats, at)
			}
		}
		h := PlanStemHandover(c.wall, c.outBar, c.inBar, downbeats, vocals, mix,
			HandoverIncoming{KeysClash: c.clash, Sings: c.sings}, StemCellSec)
		w := want.Handover
		if !near(h.Swap, w.Swap) || !near(h.BassAt, w.BassAt) || !near(h.VocalIn, w.VocalIn) || !near(h.DueAt, w.DueAt) ||
			string(h.Exit.Kind) != w.Exit.Kind || !near(h.Exit.From, w.Exit.From) || !near(h.Exit.To, w.Exit.To) ||
			!near(h.Exit.LoudDB, w.Exit.LoudDb) || (h.Exit.Held == nil) != (w.Exit.Held == nil) {
			t.Errorf("case %d:\n got  %+v\n want %+v", k, h, w)
		}
		s := FindSustain(vocals, mix, StemCellSec)
		if (s == nil) != (want.Sustain == nil) || (s != nil && (!near(s.From, want.Sustain.From) || !near(s.To, want.Sustain.To) || !near(s.HoldDB, want.Sustain.HoldDb))) {
			t.Errorf("case %d sustain: %+v, upstream %+v", k, s, want.Sustain)
		}
		last := LastVocalMoment(vocals, mix, StemCellSec)
		if (last == nil) != (want.Last == nil) || (last != nil && !near(*last, *want.Last)) {
			t.Errorf("case %d last vocal: %v, upstream %v", k, last, want.Last)
		}
		if SingsInWindow(vocals, mix) != want.Sings {
			t.Errorf("case %d sings", k)
		}
		// 上游把曲线采样成 200 点/秒的数组；这里在同样的时刻求值。
		probe := func(gain func(at float64) float64, wantValues []float64, name string) {
			points := max(2, int(math.Ceil(c.wall*200)))
			for i, frac := range []float64{0.1, 0.3, 0.5, 0.7, 0.9} {
				idx := int(jsRound(frac * float64(points-1)))
				at := float64(idx) / float64(points-1) * c.wall
				if got := gain(at); math.Abs(got-wantValues[i]) > 1e-6 {
					t.Errorf("case %d %s at %.3fs: %v, upstream %v", k, name, at, got, wantValues[i])
				}
			}
		}
		probe(func(at float64) float64 { return h.OutgoingGains(c.wall, at)[StemVocals] }, want.Probes.OutVocals, "out vocals")
		probe(func(at float64) float64 { return h.OutgoingGains(c.wall, at)[StemDrums] }, want.Probes.OutDrums, "out drums")
		probe(func(at float64) float64 { return h.IncomingGains(c.wall, at)[StemOther] }, want.Probes.InOther, "in other")
		probe(func(at float64) float64 { return h.IncomingGains(c.wall, at)[StemVocals] }, want.Probes.InVocals, "in vocals")
	}

	r := lcg(9)
	n := 4410*3 + 17
	left, right := make([]float32, n), make([]float32, n)
	for i := range left {
		left[i] = float32(float64(r()>>8)/16777216 - 0.5)
	}
	for i := range right {
		right[i] = float32(float64(r()>>8)/16777216 - 0.5)
	}
	env := EnvelopeOf([][]float32{left, right}, 44100, StemCellSec)
	if len(env) != len(g.Env) {
		t.Fatalf("envelope %d cells, upstream %d", len(env), len(g.Env))
	}
	for i := range env {
		if math.Abs(float64(env[i])-g.Env[i]) > 1e-6 {
			t.Fatalf("envelope cell %d: %v, upstream %v", i, env[i], g.Env[i])
		}
	}
}

func TestPlannerPrefersMeasuredVocalEndOverLyrics(t *testing.T) {
	to := TransitionTrack{Duration: 200}
	lyric := TransitionTrack{Duration: 200, LastSung: ptr(170.0), HasLyrics: true}
	stem := lyric
	stem.VocalEnd = ptr(190.0)
	a := PlanTransition(lyric, to, ptr(120.0), 100)
	b := PlanTransition(stem, to, ptr(120.0), 100)
	if a.Kind != KindFade || b.Kind != KindFade {
		t.Fatalf("%+v / %+v", a, b)
	}
	if !strings.Contains(b.Reason, "off the vocal stem, the lyric file said 170.00s") {
		t.Errorf("reason %q", b.Reason)
	}
	// 测得的人声更晚结束：出场尾部的纯器乐更短，过渡被它收窄。
	if b.Overlap >= a.Overlap {
		t.Errorf("vocal stem floor ignored: %v vs %v", b.Overlap, a.Overlap)
	}
}

func TestPlannerCapsBlendAtWhatTheStemsCover(t *testing.T) {
	from := TransitionTrack{Duration: 240}
	to := TransitionTrack{Duration: 200}
	free := PlanTransition(from, to, ptr(90.0), 100)
	from.Separated = &Extent{From: 234, To: 240}
	to.Separated = &Extent{From: 0, To: 30}
	capped := PlanTransition(from, to, ptr(90.0), 100)
	if free.Overlap <= 6 || capped.Overlap > 6 {
		t.Errorf("free %.2f, capped %.2f (window covers 6s)", free.Overlap, capped.Overlap)
	}
}
