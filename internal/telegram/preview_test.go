package telegram

import (
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
)

// TestPreview prints what a real message and a real card look like, so a
// change to either can be reviewed as output rather than as string
// concatenation. Run with: go test ./internal/telegram -run TestPreview -v
func TestPreview(t *testing.T) {
	t.Log("\n--- release notification ---\n" + formatRelease(notify.Release{
		ArtistName:  "あいみょん",
		Title:       "AIMYON BEST ALBUM - 唇を追え！ - (2021〜2025)",
		PrimaryType: "Album",
		ReleaseDate: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		InfoURL:     "https://musicbrainz.org/artist/908d9ac2-5187-4b9e-b281-601c6afb0791",
		Listen: notify.ListenLinks{
			Spotify:    "https://open.spotify.com/artist/5kVZa4lFUmAQlBogl1fkd6",
			YouTube:    "https://www.youtube.com/channel/UCQVhrypJhw1HxuRV4gX6hoQ",
			AppleMusic: "https://music.apple.com/jp/artist/1165017710",
		},
	}))

	card, _ := renderCandidate("drake", []musicbrainz.Artist{{
		MBID: "9fff2f8a-21e6-47de-a2b8-7f449929d43f", Name: "Drake",
		Type: "Person", Country: "CA", Disambiguation: "Canadian rapper",
		Begin: "1986-10-24", Tags: []string{"hip hop", "r&b", "pop rap"},
	}}, 0, "abcdef123456", false, time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	t.Log("\n--- artist card ---\n" + card)
}
