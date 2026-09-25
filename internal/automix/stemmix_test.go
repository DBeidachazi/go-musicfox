package automix

import (
	"math"
	"testing"
)

// sineStems 四条音轨各是一个正弦，返回窗口与它们之和。测透明性时 other 取几 kHz，
// 让出场 other 轨上 25Hz 起步的高通扫频的相移可以忽略。
func sineStems(rate, from, seconds float64, freqs [StemCount]float64, amp float64) (*StemWindow, [][2]float64) {
	n := int(seconds * rate)
	var stems [StemCount][2][]float32
	sum := make([][2]float64, n)
	for s := 0; s < StemCount; s++ {
		for ch := 0; ch < 2; ch++ {
			stems[s][ch] = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			v := float32(amp * math.Sin(2*math.Pi*freqs[s]*(from+float64(i)/rate)))
			stems[s][0][i], stems[s][1][i] = v, v
			sum[i][0] += float64(v)
			sum[i][1] += float64(v)
		}
	}
	return NewStemWindow(rate, from, RoleTail, stems), sum
}

func TestStemGestureSplicesAreTransparent(t *testing.T) {
	const rate = 44100.0
	outWin, outMix := sineStems(rate, 170, 30, [StemCount]float64{110, 55, 5000, 440}, 0.1)
	inWin, inMix := sineStems(rate, 0, 30, [StemCount]float64{130, 65, 6000, 520}, 0.1)
	inWin.Role = RoleHead
	outStart, inStart, wall := 180.0, 2.0, 12.0
	g, why := PlanStemGesture(StemGestureRequest{Out: outWin, In: inWin, OutStart: outStart, InStart: inStart, Wall: wall, Rate: rate})
	if g == nil {
		t.Fatal(why)
	}
	blend := StyledBlend{Style: StylePlainBlend, Overlap: wall, Crossover: 0.5}
	m := NewMixer(MixerConfig{SampleRate: rate, Blend: blend, Plan: TransitionPlan{EchoThrow: true}, Stems: g})
	if !m.StemsActive() || m.echoOn || m.outTone != nil {
		t.Fatal("stem gesture should replace bands and echo")
	}
	n := int(wall * rate)
	outAt, inAt := int((outStart-170)*rate), int(inStart*rate)
	dst := make([][2]float64, n+100)
	m.Process(dst, outMix[outAt:outAt+n+100], inMix[inAt:inAt+n+100])

	// 窗口开头：出场四轨全在（鼓未换、人声未退、进场 other 还没进来）→ 等于出场整轨。
	for i := 0; i < int(0.5*rate); i++ {
		if d := math.Abs(dst[i][0] - SoftLimit(outMix[outAt+i][0])); d > 2e-3 {
			t.Fatalf("start sample %d off by %g", i, d)
		}
	}
	// 结尾拼回整轨：出场 other 床一直淡到窗口最后一刻（上游如此设计），
	// 所以只有最后的拼接段及之后才是纯进场整轨。
	for i := n - int(math.Round(StemSpliceSec*rate)); i < n+100; i++ {
		if d := math.Abs(dst[i][0] - SoftLimit(inMix[inAt+i][0])); d > 2e-3 {
			t.Fatalf("end sample %d (of %d) off by %g", i, n, d)
		}
	}
	// 窗口中段（鼓已换、进场人声已到、出场 other 尚未开始淡）：只剩出场 other 与进场四轨。
	h := g.Handover
	mid := int((h.VocalIn + 0.6) * rate)
	if at := float64(mid) / rate; at < wall*0.92 {
		want := inMix[inAt+mid][0] + outWin.Sample(StemOther, outAt+mid)[0]
		if d := math.Abs(dst[mid][0] - SoftLimit(want)); d > 0.05 {
			t.Errorf("mid-window at %.2fs off by %g", at, d)
		}
	}
	if g.Handover.Swap <= 1 || g.Handover.Swap >= wall-1.5 || g.Handover.BassAt <= g.Handover.Swap {
		t.Errorf("handover %+v", g.Handover)
	}
}

func TestStemGestureRefusesUncoveredWindow(t *testing.T) {
	outWin, _ := sineStems(44100, 170, 30, [StemCount]float64{1, 2, 3, 4}, 0.1)
	inWin, _ := sineStems(44100, 0, 30, [StemCount]float64{1, 2, 3, 4}, 0.1)
	for _, c := range []struct{ outStart, inStart, wall float64 }{
		{165, 0, 10},  // 早于出场窗口
		{195, 0, 10},  // 越过出场窗口尾
		{180, 25, 10}, // 越过进场窗口尾
	} {
		if g, _ := PlanStemGesture(StemGestureRequest{Out: outWin, In: inWin, OutStart: c.outStart, InStart: c.inStart, Wall: c.wall}); g != nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestStemGestureResamplesToOutputRate(t *testing.T) {
	outWin, _ := sineStems(44100, 170, 30, [StemCount]float64{110, 55, 1000, 440}, 0.1)
	inWin, _ := sineStems(44100, 0, 30, [StemCount]float64{130, 65, 1200, 520}, 0.1)
	g, why := PlanStemGesture(StemGestureRequest{Out: outWin, In: inWin, OutStart: 180, InStart: 1, Wall: 8, Rate: 48000})
	if g == nil {
		t.Fatal(why)
	}
	if g.Out.Rate != 48000 || g.In.Rate != 48000 || g.OutAt != 480000 || g.InAt != 48000 {
		t.Errorf("rate %v/%v at %d/%d", g.Out.Rate, g.In.Rate, g.OutAt, g.InAt)
	}
	// 同一时刻的采样值在重采样前后一致（1kHz other 轨）。
	a := outWin.Sample(StemOther, 441000)[0]
	b := g.Out.Sample(StemOther, 480000)[0]
	if math.Abs(a-b) > 2e-3 {
		t.Errorf("resampled %v vs %v", a, b)
	}
}

func TestStemWindowRoundTripsThroughInt16(t *testing.T) {
	var stems [StemCount][2][]float32
	for s := range stems {
		stems[s] = [2][]float32{{0, 0.5, -1.7, 0.25}, {1.2, -0.3, 0, 0}}
	}
	w := NewStemWindow(44100, 0, RoleHead, stems)
	// 峰值除数：过冲到 1.7 也不被削平。
	if v := w.Sample(StemOther, 2)[0]; math.Abs(v+1.7) > 1e-3 {
		t.Errorf("overshoot clipped to %v", v)
	}
	if v := w.Sample(StemDrums, 99); v != [2]float64{} {
		t.Errorf("out of range %v", v)
	}
}
