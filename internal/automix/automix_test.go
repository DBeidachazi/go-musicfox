package automix

import (
	"math"
	"strings"
	"testing"
)

// clickTrack 合成一条带底鼓/军鼓/和弦的节拍音轨：bpm 速度，第一拍在 firstBeat 秒，
// 小节第一拍底鼓更重，每小节换一个和弦。
func clickTrack(bpm, seconds, firstBeat float64, rate float64) []float32 {
	n := int(seconds * rate)
	out := make([]float32, n)
	period := 60 / bpm
	chords := [][]float64{{261.6, 329.6, 392}, {220, 261.6, 329.6}, {174.6, 220, 261.6}, {196, 246.9, 293.7}}
	for i := range out {
		t := float64(i) / rate
		if t < firstBeat {
			continue
		}
		beat := int((t - firstBeat) / period)
		since := t - firstBeat - float64(beat)*period
		bar := beat / 4
		// 每拍等强的 1kHz 衰减点击（与上游 trackProfile.test.ts 的合成信号一致）。
		var v float64
		if since < 0.01 {
			v = (1 - since/0.01) * math.Sin(2*math.Pi*1000*since)
		}
		for _, f := range chords[bar%len(chords)] {
			v += 0.05 * math.Sin(2*math.Pi*f*t)
		}
		out[i] = float32(v)
	}
	return out
}

func TestEstimateTempoOnClickTrack(t *testing.T) {
	for _, bpm := range []float64{92, 110, 124, 140} {
		mono := clickTrack(bpm, 60, 0.37, ProfileSampleRate)
		p := AnalyseTrack(mono, ProfileSampleRate, AnalyseOptions{})
		if p == nil || p.BPM == nil {
			t.Fatalf("%v BPM: no tempo", bpm)
		}
		if got := *p.BPM; math.Abs(got-bpm)/bpm > 0.01 {
			t.Errorf("%v BPM: measured %.2f", bpm, got)
		}
		// 网格锚定在曲尾（上游 README §6.1），只检查最后 10 秒。
		//
		// 容差 0.1s 而非 1/8 拍：上游把拍时刻记作 envelope 下标 × hop + FFT/2，而 flux 峰值出现在
		// 起音刚进入窗口时、且 envelope[i] 对应第 i+1 帧，于是网格系统性早约 70-80ms。
		// alignEntry 用的是两条同法网格之差，偏差相互抵消；这里保持与上游一致，不单方面修正。
		truePeriod, period := 60/bpm, 60 / *p.BPM
		for at := 50.0; at < 59; at += 1.3 {
			line := p.BeatOffset + math.Round((at-p.BeatOffset)/period)*period
			beat := 0.37 + math.Round((line-0.37)/truePeriod)*truePeriod
			if d := math.Abs(line - beat); d > 0.1 {
				t.Errorf("%v BPM: grid line %.3f is %.3fs from the beat at %.3f", bpm, line, d, beat)
				break
			}
		}
	}
}

func TestAnalyseTrackEdges(t *testing.T) {
	rate := float64(ProfileSampleRate)
	body := clickTrack(120, 40, 0, rate)
	// 2 秒前导静音 + 40 秒 + 3 秒尾部静音
	mono := make([]float32, int(2*rate))
	mono = append(mono, body...)
	mono = append(mono, make([]float32, int(3*rate))...)
	p := AnalyseTrack(mono, rate, AnalyseOptions{})
	if p == nil {
		t.Fatal("no profile")
	}
	if math.Abs(p.LeadIn-2) > 0.1 {
		t.Errorf("leadIn %.3f, want ~2", p.LeadIn)
	}
	if p.LeadOut == nil || math.Abs(*p.LeadOut-3) > 0.3 {
		t.Errorf("leadOut %v, want ~3", p.LeadOut)
	}
	if p.EndsHot == nil || !*p.EndsHot || !p.StartsHot {
		t.Errorf("a track at full level at both ends: startsHot=%v endsHot=%v", p.StartsHot, p.EndsHot)
	}
}

func TestKeyFromChroma(t *testing.T) {
	// C 大调音阶的色度
	chroma := make([]float64, 12)
	for _, pc := range []int{0, 2, 4, 5, 7, 9, 11} {
		chroma[pc] = 1
	}
	chroma[0], chroma[7], chroma[4] = 3, 2, 2
	k := KeyFromChroma(chroma)
	if k.Key != 0 || !k.Major {
		t.Errorf("got key %d major=%v", k.Key, k.Major)
	}
}

func TestCrossfadeGainsKeepPower(t *testing.T) {
	for _, together := range []float64{0, 0.55} {
		for i := 0; i <= 100; i++ {
			p := float64(i) / 100
			out, in := CrossfadeGains(p, 0.45, together)
			power := out*out + in*in
			want := math.Pow(DbToGain(-BlendHeadroomDB*headroomBell(p)), 2)
			if math.Abs(power-want) > 1e-9 {
				t.Fatalf("together=%v p=%v power %.6f want %.6f", together, p, power, want)
			}
		}
		if out, in := CrossfadeGains(0, 0.45, together); out != 1 || in != 0 {
			t.Errorf("start not at unity: %v %v", out, in)
		}
		if out, in := CrossfadeGains(1, 0.45, together); math.Abs(out) > 1e-12 || math.Abs(in-1) > 1e-12 {
			t.Errorf("end not at unity: %v %v", out, in)
		}
	}
}

func TestQuantiseToMusic(t *testing.T) {
	beat := 0.5
	if got := QuantiseToMusic(7.9, &beat, 25); got != 8 { // 16 beats = one phrase
		t.Errorf("got %v", got)
	}
	// 天花板拦截时往下退整数小节，而不是把上限本身当答案
	if got := QuantiseToMusic(16, &beat, 7); got != 7 {
		t.Errorf("got %v, want 7", got)
	}
}

func TestSettledBPM(t *testing.T) {
	w, o := 125.0, 63.0
	if got := SettledBPM(&w, &o); got == nil || math.Abs(*got-126) > 0.01 {
		t.Errorf("an octave apart is one tempo, in the whole track's counting: %v", got)
	}
	w, o = 92, 123
	if got := SettledBPM(&w, &o); got != nil {
		t.Errorf("4:3 apart is not a tempo: %v", *got)
	}
}

func testProfile(bpm float64) *TrackProfile {
	mono := clickTrack(bpm, 90, 0.5, ProfileSampleRate)
	return AnalyseTrack(mono, ProfileSampleRate, AnalyseOptions{})
}

func TestPlanTransitionIsMusical(t *testing.T) {
	from, to := testProfile(120), testProfile(122)
	sung := 60.0
	plan := PlanTransition(
		TransitionTrack{Duration: from.Duration, Profile: from, LastSung: &sung, HasLyrics: true},
		TransitionTrack{Duration: to.Duration, Profile: to},
		SettledBPM(from.BPM, from.OutroBPM), 30,
	)
	t.Log(plan.Reason)
	if plan.Kind != KindFade {
		t.Fatalf("expected a fade, got %s: %s", plan.Kind, plan.Reason)
	}
	if plan.OutStart+plan.Overlap > from.Duration+1e-6 || plan.OutStart < 30 {
		t.Errorf("blend %.2f+%.2f outside [30, %.2f]", plan.OutStart, plan.Overlap, from.Duration)
	}
	if plan.Overlap < MinOverlapSec || plan.Overlap > MaxOverlapSec {
		t.Errorf("overlap %.2f", plan.Overlap)
	}
}

func TestPlanForModeCrossfadeAndNoEvidence(t *testing.T) {
	from := TransitionTrack{Duration: 200}
	to := TransitionTrack{Duration: 180}
	plan := PlanForMode(Settings{Mode: ModeCrossfade, CrossfadeSeconds: 8}, from, to, nil, 100)
	if plan.Kind != KindFade || plan.Overlap != 8 || plan.OutStart != 192 {
		t.Errorf("crossfade plan: %+v", plan)
	}
	plan = PlanForMode(Settings{Mode: ModeAutomix}, from, to, nil, 100)
	if !strings.Contains(plan.Reason, "no evidence") || plan.Overlap != CrossfadeDefaultSec {
		t.Errorf("no-evidence plan: %+v", plan)
	}
	plan = PlanForMode(Settings{Mode: ModeAutomix}, TransitionTrack{Duration: math.Inf(1)}, to, nil, 0)
	if plan.Kind != KindHardCut {
		t.Errorf("unknown duration should be a hard cut: %+v", plan)
	}
}

func TestMixerRunsTheWholeBlend(t *testing.T) {
	rate := 44100.0
	for _, style := range []TransitionStyle{StyleBassSwap, StyleTailRide, StylePlainBlend, StyleBeatCut} {
		period := 0.5
		next := 0.2
		shape := ShapeBlend(BlendShapeRequest{Style: style, Room: 4, Overlap: 4, Crossover: 0.45,
			NextBeatIn: &next, PeriodSec: &period, IncomingLeadIn: ptr(1.0)})
		plan := TransitionPlan{Style: style, EchoThrow: style == StyleBeatCut, TiltDB: [2]float64{2, -2}}
		m := NewMixer(MixerConfig{SampleRate: rate, Blend: shape, Plan: plan, TrimDB: 3, PeriodSec: &period})
		a := make([][2]float64, 1024)
		b := make([][2]float64, 1024)
		dst := make([][2]float64, 1024)
		var i int
		peak := 0.0
		for blocks := 0; !m.Done(); blocks++ {
			if blocks > 10000 {
				t.Fatalf("%s: mixer never finished", style)
			}
			for j := range a {
				x := float64(i+j) / rate
				a[j] = [2]float64{0.9 * math.Sin(2*math.Pi*80*x), 0.9 * math.Sin(2*math.Pi*80*x)}
				b[j] = [2]float64{0.9 * math.Sin(2*math.Pi*3000*x), 0.9 * math.Sin(2*math.Pi*3000*x)}
			}
			m.Process(dst, a, b)
			for _, s := range dst {
				if math.IsNaN(s[0]) || math.IsInf(s[0], 0) {
					t.Fatalf("%s: NaN in output", style)
				}
				peak = math.Max(peak, math.Abs(s[0]))
			}
			i += len(a)
		}
		if peak > 1 {
			t.Errorf("%s: output peak %.3f exceeds full scale", style, peak)
		}
	}
}

func TestSoftLimit(t *testing.T) {
	if SoftLimit(0.5) != 0.5 || SoftLimit(-0.95) != -0.95 {
		t.Error("soft limit must be transparent below the knee")
	}
	if v := SoftLimit(3); v > 1 || v <= 0.95 {
		t.Errorf("limit(3) = %v", v)
	}
	if SoftLimit(-2) != -SoftLimit(2) {
		t.Error("not odd-symmetric")
	}
}
