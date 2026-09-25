package player

import (
	"math"
	"os"
	"testing"

	"github.com/go-musicfox/go-musicfox/internal/automix"
	"github.com/go-musicfox/go-musicfox/internal/configs"
)

// 分离窗口必须与播放解码逐采样对齐，否则四轨与整轨在拼接处错开就是一声回音。
func TestStemWindowAlignsWithPlaybackDecode(t *testing.T) {
	// 仓库里唯一够长（>30s）的 MP3。
	const path = "../../website/audio.mp3"
	if _, err := os.Stat(path); err != nil {
		t.Skip(err)
	}
	if configs.AppConfig == nil {
		configs.AppConfig = &configs.Config{}
		t.Cleanup(func() { configs.AppConfig = nil })
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, format, err := decodeSong(Mp3, file, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var full [][2]float64
	buf := make([][2]float64, 4096)
	for {
		n, ok := raw.Stream(buf)
		full = append(full, buf[:n]...)
		if !ok || n == 0 {
			break
		}
	}
	_ = raw.Close()
	for _, role := range []automix.StemRole{automix.RoleHead, automix.RoleTail} {
		window, rate, start, err := readStemWindow(path, URLMusic{Type: Mp3}, role)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if rate != format.SampleRate || len(window) != int(automix.StemWindowSec*float64(rate)) {
			t.Fatalf("%s: %d samples at %d Hz", role, len(window), rate)
		}
		if role == automix.RoleTail && start+len(window) != len(full) {
			t.Errorf("tail window ends at %d, track has %d samples", start+len(window), len(full))
		}
		for i, s := range window {
			if s != full[start+i] {
				t.Fatalf("%s window sample %d (track %d) is %v, playback decode has %v", role, i, start+i, s, full[start+i])
			}
		}
		left, _, from, err := decodeStemWindow(path, URLMusic{Type: Mp3}, role)
		if err != nil || math.Abs(from-float64(start)/float64(rate)) > 1e-9 ||
			math.Abs(float64(len(left))-automix.StemWindowSec*automix.StemSampleRate) > 64 {
			t.Errorf("%s at 44.1kHz: %d samples from %.3fs (%v)", role, len(left), from, err)
		}
	}
}
