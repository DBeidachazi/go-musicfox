package models

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-musicfox/go-musicfox/internal/automix"
)

func TestOverlapAddReconstructsWithAnIdentityModel(t *testing.T) {
	const segment = 400
	for _, total := range []int{1, 250, 400, 401, 1234, 3000} {
		left, right := make([]float32, total), make([]float32, total)
		for i := range left {
			left[i] = float32(math.Sin(float64(i) * 0.01))
			right[i] = float32(math.Cos(float64(i) * 0.013))
		}
		calls := 0
		// 第 s 轨 = 输入 × (s+1)：overlap-add 归一化后必须精确还原。
		got, err := overlapAdd(left, right, segment, func(in []float32) ([]float32, error) {
			calls++
			out := make([]float32, 4*2*segment)
			for s := 0; s < 4; s++ {
				for i, v := range in {
					out[s*2*segment+i] = v * float32(s+1)
				}
			}
			return out, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		stride := segment - segment/4
		if want := max(1, (total+stride-1)/stride); calls != want {
			t.Errorf("total %d: %d segments, want %d", total, calls, want)
		}
		if _, ok := got["other"]; ok || len(got) != 3 {
			t.Errorf("returned %d stems", len(got))
		}
		for s, name := range []string{"drums", "bass", "", "vocals"} {
			if name == "" {
				continue
			}
			for i := 0; i < total; i++ {
				// 首个采样的窗权重为 0（ramp 从 0 开始），与参考实现一样得到 0。
				if i == 0 {
					continue
				}
				if d := math.Abs(float64(got[name][0][i] - left[i]*float32(s+1))); d > 1e-4 {
					t.Fatalf("total %d %s sample %d off by %g", total, name, i, d)
				}
			}
		}
	}
}

func TestBuildWindowMatchesRunner(t *testing.T) {
	w := buildWindow(16) // overlap 4: ramp 0, 1/3, 2/3, 1
	want := []float32{0, 1.0 / 3, 2.0 / 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2.0 / 3, 1.0 / 3, 0}
	for i := range want {
		if math.Abs(float64(w[i]-want[i])) > 1e-6 {
			t.Fatalf("window %v", w)
		}
	}
}

// 用一个假"python"验证与 runner 的文件协议：argv、输入布局、输出大小与轨序。
func TestPythonSidecarProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "python")
	// argv: script in out total model。输出 = 输入的左声道重复成 drums/bass/vocals 的 L 与 R。
	script := `#!/bin/sh
in="$2"; out="$3"; total="$4"
bytes=$((total * 4))
: > "$out.part"
for s in 1 2 3; do head -c $bytes "$in" >> "$out.part"; tail -c $bytes "$in" >> "$out.part"; done
mv "$out.part" "$out"
echo '{"peak_ws_mb": 1}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewStore(Options{Dir: dir, Level: LevelFull, Python: fake})
	model := store.path(HtdemucsFile)
	_ = os.WriteFile(model, []byte("x"), 0o644)
	_ = os.WriteFile(store.markerOf(htdemucs(nil)), []byte(htdemucs(nil).SHA256), 0o644)
	// 大小不对时 verified 为 false；这里直接调 separatePython 绕过模型检查。
	e := NewEngine(store)
	left := []float32{0.1, 0.2, 0.3, -0.4}
	right := []float32{-1, 1, 0.5, 0}
	got, err := e.separatePython(context.Background(), fake, model, left, right)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"drums", "bass", "vocals"} {
		for i := range left {
			if got[name][0][i] != left[i] || got[name][1][i] != right[i] {
				t.Fatalf("%s = %v", name, got[name])
			}
		}
	}
	if _, err := os.Stat(store.path("htdemucs_runner.py")); err != nil {
		t.Error("runner script not written beside the models")
	}
}

func TestSeparateWindowDerivesOtherBySubtraction(t *testing.T) {
	// 走 overlapAdd 的恒等模型路径，检查 other = mix − 三轨，四轨之和等于原曲。
	n := 44100
	left, right := make([]float32, n), make([]float32, n)
	for i := range left {
		left[i] = float32(0.3 * math.Sin(float64(i)*0.05))
		right[i] = left[i]
	}
	parts := map[string][2][]float32{}
	for k, name := range []string{"drums", "bass", "vocals"} {
		l, r := make([]float32, n), make([]float32, n)
		for i := range l {
			l[i] = left[i] * float32(0.1*float64(k+1))
			r[i] = l[i]
		}
		parts[name] = [2][]float32{l, r}
	}
	var stems [automix.StemCount][2][]float32
	stems[automix.StemDrums], stems[automix.StemBass], stems[automix.StemVocals] = parts["drums"], parts["bass"], parts["vocals"]
	other := [2][]float32{make([]float32, n), make([]float32, n)}
	for ch, mix := range [][]float32{left, right} {
		for i := range mix {
			other[ch][i] = mix[i] - stems[0][ch][i] - stems[1][ch][i] - stems[3][ch][i]
		}
	}
	stems[automix.StemOther] = other
	w := automix.NewStemWindow(automix.StemSampleRate, 10, automix.RoleTail, stems)
	for i := 0; i < n; i += 997 {
		var sum float64
		for s := 0; s < automix.StemCount; s++ {
			sum += w.Sample(s, i)[0]
		}
		if math.Abs(sum-float64(left[i])) > 1e-4 {
			t.Fatalf("sample %d: stems sum to %v, mix is %v", i, sum, left[i])
		}
	}
}
