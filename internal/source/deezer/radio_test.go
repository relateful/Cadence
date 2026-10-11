package deezer

import (
	"context"
	"testing"

	"github.com/greed/cadence/internal/source"
)

func TestMixSongBecomesAPlayableTrack(t *testing.T) {
	got := mixSong{ID: "123", Title: "Sweater Weather", Version: "(Remastered)", Artist: "The Neighbourhood", Album: "I Love You.", Picture: "abc", Duration: "240", ISRC: "USSM11204960"}.toTrack()
	want := source.Track{
		Source:     Name,
		Identifier: "123",
		Title:      "Sweater Weather (Remastered)",
		Author:     "The Neighbourhood",
		Album:      "I Love You.",
		DurationMs: 240_000,
		ISRC:       "USSM11204960",
		ArtworkURL: coverPrefix + "abc" + coverSuffix,
		URI:        "https://www.deezer.com/track/123",
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestMixSongDoesNotRepeatAVersionAlreadyInTheTitle(t *testing.T) {
	got := mixSong{ID: "1", Title: "Song (Live)", Version: "(Live)", Artist: "A"}.toTrack()
	if got.Title != "Song (Live)" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.ArtworkURL != "" || got.DurationMs != 0 {
		t.Fatalf("missing picture and duration must stay empty, got %+v", got)
	}
}

func TestWithoutSeedDropsTheSeedAndDuplicates(t *testing.T) {
	in := []source.Track{{Identifier: "1"}, {Identifier: "2"}, {Identifier: "2"}, {Identifier: "3"}}
	got := withoutSeed(in, "1")
	if len(got) != 2 || got[0].Identifier != "2" || got[1].Identifier != "3" {
		t.Fatalf("got %+v, want [2 3]", got)
	}
}

func TestCapTracks(t *testing.T) {
	in := []source.Track{{Identifier: "1"}, {Identifier: "2"}, {Identifier: "3"}}
	if got := capTracks(in, 2); len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got := capTracks(in, 10); len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
}

func TestRecommendRejectsASeedThatIsNotADeezerID(t *testing.T) {
	d := testDeezer(nil, RotationFailover)
	if _, err := d.Recommend(context.Background(), "soundcloud-track", 10); err == nil {
		t.Fatal("a non-numeric seed must be rejected before any request")
	}
}

func TestRecommendServesFromTheCacheWithoutCallingDeezer(t *testing.T) {
	d := testDeezer(nil, RotationFailover)
	d.radio.Set("42", []source.Track{{Identifier: "7"}, {Identifier: "8"}, {Identifier: "9"}})
	got, err := d.Recommend(context.Background(), "42", 2)
	if err != nil || len(got) != 2 || got[0].Identifier != "7" {
		t.Fatalf("got %+v, %v; want the first two cached tracks", got, err)
	}
}
