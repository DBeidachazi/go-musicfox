package configs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultBeepCrossfadeConfig(t *testing.T) {
	config, err := NewConfigFromTomlFile(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Player.Beep.Crossfade {
		t.Fatal("crossfade is enabled by default")
	}
	if got, want := config.Player.Beep.CrossfadeDuration, 5; got != want {
		t.Fatalf("crossfade duration = %d, want %d", got, want)
	}
}

func TestCustomBeepCrossfadeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[player.beep]\ncrossfade = true\ncrossfadeDuration = 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := NewConfigFromTomlFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Player.Beep.Crossfade {
		t.Fatal("custom crossfade setting was not loaded")
	}
	if got, want := config.Player.Beep.CrossfadeDuration, 7; got != want {
		t.Fatalf("crossfade duration = %d, want %d", got, want)
	}
}
