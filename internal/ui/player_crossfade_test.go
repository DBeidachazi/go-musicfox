package ui

import (
	"testing"
	"time"

	playerpkg "github.com/go-musicfox/go-musicfox/internal/player"
	"github.com/go-musicfox/go-musicfox/internal/playlist"
	"github.com/go-musicfox/go-musicfox/internal/structs"
	"github.com/go-musicfox/go-musicfox/internal/types"
)

type crossfadeTestPlayer struct {
	playerpkg.Player
	music      playerpkg.URLMusic
	duration   time.Duration
	generation int64
	pending    bool
}

func (p *crossfadeTestPlayer) PlayCrossfade(playerpkg.URLMusic) {}

func (p *crossfadeTestPlayer) CancelCrossfade() {}

func (p *crossfadeTestPlayer) CurMusic() playerpkg.URLMusic {
	return p.music
}

func (p *crossfadeTestPlayer) State() types.State {
	return types.Playing
}

func (p *crossfadeTestPlayer) CrossfadeDuration() time.Duration {
	return p.duration
}

func (p *crossfadeTestPlayer) CrossfadePending() bool {
	return p.pending
}

func TestCrossfadePreparingNextSongUsesBackendAndQueueIdentity(t *testing.T) {
	backend := &crossfadeTestPlayer{
		music:   playerpkg.URLMusic{Song: structs.Song{Id: 1}},
		pending: true,
	}
	p := &Player{Player: backend, playlistManager: playlist.NewPlaylistManager()}
	p.InitSongManager(1, []structs.Song{{Id: 1}, {Id: 2}})
	if !p.crossfadePreparingNextSong() {
		t.Fatal("pending next track was not recognized as preparation")
	}

	backend.music.Song.Id = 2
	if p.crossfadePreparingNextSong() {
		t.Fatal("active next track was mistaken for preparation")
	}
}

func (p *crossfadeTestPlayer) CrossfadeGeneration() int64 {
	return p.generation
}

func TestShouldStartCrossfadeOncePerTrack(t *testing.T) {
	backend := &crossfadeTestPlayer{
		music:    playerpkg.URLMusic{Song: structs.Song{Id: 1, Duration: 3 * time.Minute}},
		duration: 5 * time.Second,
	}
	p := &Player{Player: backend}

	if p.shouldStartCrossfade(174 * time.Second) {
		t.Fatal("crossfade started before the configured window")
	}
	if !p.shouldStartCrossfade(175 * time.Second) {
		t.Fatal("crossfade did not start at the configured window")
	}
	if p.shouldStartCrossfade(176 * time.Second) {
		t.Fatal("crossfade started twice for the same track")
	}

	backend.music = playerpkg.URLMusic{Song: structs.Song{Id: 2, Duration: 4 * time.Minute}}
	if p.shouldStartCrossfade(time.Second) {
		t.Fatal("crossfade started at the beginning of the next track")
	}
	if !p.shouldStartCrossfade(235 * time.Second) {
		t.Fatal("crossfade did not reset for the next track")
	}
}

func TestSeekIntoCrossfadeWindowSuppressesCurrentTrack(t *testing.T) {
	backend := &crossfadeTestPlayer{
		music:    playerpkg.URLMusic{Song: structs.Song{Id: 1, Duration: 3 * time.Minute}},
		duration: 5 * time.Second,
	}
	p := &Player{Player: backend}

	if p.shouldStartCrossfade(170 * time.Second) {
		t.Fatal("crossfade started before the configured window")
	}
	p.noteCrossfadeSeek(178 * time.Second)
	if p.shouldStartCrossfade(179 * time.Second) {
		t.Fatal("seek into the crossfade window triggered a transition")
	}
	if p.shouldStartCrossfade(179500 * time.Millisecond) {
		t.Fatal("suppression did not last until the end of the track")
	}
}

func TestSeekBeforeCrossfadeWindowRearmsCurrentTrack(t *testing.T) {
	backend := &crossfadeTestPlayer{
		music:    playerpkg.URLMusic{Song: structs.Song{Id: 1, Duration: 3 * time.Minute}},
		duration: 5 * time.Second,
	}
	p := &Player{Player: backend}

	p.noteCrossfadeSeek(178 * time.Second)
	p.noteCrossfadeSeek(170 * time.Second)
	if p.shouldStartCrossfade(174 * time.Second) {
		t.Fatal("crossfade started before the configured window")
	}
	if !p.shouldStartCrossfade(175 * time.Second) {
		t.Fatal("seek before the crossfade window did not rearm the transition")
	}
}

func TestSeekWhilePreparingDefersSelectedTrackToEOF(t *testing.T) {
	p := &Player{}
	p.suppressPreparedCrossfadeAfterSeek()
	if !p.crossfadeSeekSuppressed {
		t.Fatal("seek did not suppress another transition for the current track")
	}
	p.crossfadeMu.Lock()
	playDirect := p.crossfadePlayNextDirect
	p.crossfadeMu.Unlock()
	if !playDirect {
		t.Fatal("selected next track was not retained for direct playback")
	}
}

func TestSameTrackReplayRearmsCrossfade(t *testing.T) {
	backend := &crossfadeTestPlayer{
		music:    playerpkg.URLMusic{Song: structs.Song{Id: 1, Duration: 3 * time.Minute}},
		duration: 5 * time.Second,
	}
	p := &Player{Player: backend}

	if p.shouldStartCrossfade(174*time.Second) || !p.shouldStartCrossfade(175*time.Second) {
		t.Fatal("first playback did not trigger at the crossfade boundary")
	}
	if p.shouldStartCrossfade(time.Second) {
		t.Fatal("replayed track triggered at its beginning")
	}
	if p.shouldStartCrossfade(174 * time.Second) {
		t.Fatal("replayed track triggered before the crossfade boundary")
	}
	if !p.shouldStartCrossfade(175 * time.Second) {
		t.Fatal("replayed track did not rearm the crossfade")
	}
}
