package player

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-musicfox/go-musicfox/internal/automix/models"
	"github.com/go-musicfox/go-musicfox/internal/configs"
)

// TestAutomixAnalyseRealFiles 在真实文件上测分析耗时与结果。
// 设置 AUTOMIX_BENCH_DIR 指向一个装有音频文件的目录才运行；
// 再设置 MUSICFOX_AUTOMIX_MODELS 指向已下载的模型目录，则同时测 Beat This!（L1）的耗时。
func TestAutomixAnalyseRealFiles(t *testing.T) {
	dir := os.Getenv("AUTOMIX_BENCH_DIR")
	if dir == "" {
		t.Skip("AUTOMIX_BENCH_DIR not set")
	}
	if configs.AppConfig == nil {
		configs.AppConfig = &configs.Config{}
		t.Cleanup(func() { configs.AppConfig = nil })
	}
	if configs.AppConfig == nil {
		configs.AppConfig = &configs.Config{}
		t.Cleanup(func() { configs.AppConfig = nil })
	}
	var engine *models.Engine
	if modelsDir := os.Getenv("MUSICFOX_AUTOMIX_MODELS"); modelsDir != "" {
		engine = newBenchEngine(modelsDir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total, audio time.Duration
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		started := time.Now()
		p, err := analyseFile(path, URLMusic{Type: Mp3}, engine)
		took := time.Since(started)
		if err != nil {
			t.Logf("%s: %v", e.Name(), err)
			continue
		}
		count++
		total += took
		audio += time.Duration(p.Duration * float64(time.Second))
		t.Logf("%s: %v  %s", e.Name(), took.Round(time.Millisecond), describeProfile(p))
	}
	if count > 0 {
		t.Logf("%d files, %v audio analysed in %v (%.0fx realtime, %v per track)",
			count, audio.Round(time.Second), total.Round(time.Millisecond),
			audio.Seconds()/total.Seconds(), (total / time.Duration(count)).Round(time.Millisecond))
	}
}

func newBenchEngine(dir string) *models.Engine {
	return models.NewEngine(models.NewStore(models.Options{Dir: dir, Level: models.LevelBeat}))
}
