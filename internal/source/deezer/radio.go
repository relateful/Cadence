package deezer

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/source"
)

const (
	radioEntries  = 20_000
	radioTTL      = 6 * time.Hour
	radioMaxLimit = 50
	coverPrefix   = "https://cdn-images.dzcdn.net/images/cover/"
	coverSuffix   = "/1000x1000-000000-80-0-0.jpg"
)

type mixSong struct {
	ID       string `json:"SNG_ID"`
	Title    string `json:"SNG_TITLE"`
	Version  string `json:"VERSION"`
	Artist   string `json:"ART_NAME"`
	Album    string `json:"ALB_TITLE"`
	Picture  string `json:"ALB_PICTURE"`
	Duration string `json:"DURATION"`
	ISRC     string `json:"ISRC"`
}

type mixResults struct {
	Data []mixSong `json:"data"`
}

func (s mixSong) toTrack() source.Track {
	title := s.Title
	if v := strings.TrimSpace(s.Version); v != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(v)) {
		title += " " + v
	}
	seconds, _ := strconv.ParseInt(s.Duration, 10, 64)
	art := ""
	if s.Picture != "" {
		art = coverPrefix + s.Picture + coverSuffix
	}
	return source.Track{
		Source:     Name,
		Identifier: s.ID,
		Title:      title,
		Author:     s.Artist,
		Album:      s.Album,
		DurationMs: seconds * 1000,
		ISRC:       s.ISRC,
		ArtworkURL: art,
		URI:        "https://www.deezer.com/track/" + s.ID,
	}
}

func (d *Deezer) Recommend(ctx context.Context, identifier string, limit int) ([]source.Track, error) {
	limit = max(1, min(radioMaxLimit, limit))
	seed, err := d.seedID(ctx, strings.TrimSpace(identifier))
	if err != nil {
		return nil, err
	}
	if hit, ok := d.radio.Get(seed); ok {
		return capTracks(hit, limit), nil
	}
	source := "track-mix"
	tracks, err := d.trackMix(ctx, seed)
	if err != nil || len(tracks) == 0 {
		if err != nil {
			d.log.Warn("track mix failed, using artist radio", "track", seed, "error", err)
		}
		source = "artist-radio"
		if tracks, err = d.artistRadio(ctx, seed); err != nil {
			metrics.Recommendations.Inc("failed")
			d.log.Warn("no recommendations: artist radio failed too", "track", seed, "error", err)
			return nil, err
		}
	}
	tracks = withoutSeed(tracks, seed)
	if len(tracks) == 0 {
		metrics.Recommendations.Inc("empty")
		return nil, nil
	}
	metrics.Recommendations.Inc(source)
	d.radio.Set(seed, tracks)
	return capTracks(tracks, limit), nil
}

func (d *Deezer) seedID(ctx context.Context, identifier string) (string, error) {
	if isrc, ok := strings.CutPrefix(identifier, "isrc:"); ok {
		t, err := d.ByISRC(ctx, source.NormalizeISRC(isrc))
		if err != nil {
			return "", err
		}
		return t.Identifier, nil
	}
	if _, err := strconv.ParseInt(identifier, 10, 64); err != nil {
		return "", source.Fail(source.SeverityCommon, Name, "not a deezer track id", nil)
	}
	return identifier, nil
}

func (d *Deezer) trackMix(ctx context.Context, seed string) ([]source.Track, error) {
	if d.accounts.empty() {
		return nil, nil
	}
	return firstAccount(ctx, d, func(a *account) ([]source.Track, error) {
		s, err := d.session(ctx, a)
		if err != nil {
			return nil, err
		}
		var res mixResults
		if err := d.gw(ctx, a, s, "song.getSearchTrackMix", map[string]any{"sng_id": seed, "start_with_input_track": false}, &res); err != nil {
			return nil, err
		}
		out := make([]source.Track, 0, len(res.Data))
		for _, song := range res.Data {
			if song.ID != "" && song.Title != "" && song.Artist != "" {
				out = append(out, song.toTrack())
			}
		}
		return out, nil
	})
}

func (d *Deezer) artistRadio(ctx context.Context, seed string) ([]source.Track, error) {
	var t apiTrack
	if err := d.api(ctx, "/track/"+url.PathEscape(seed), nil, &t); err != nil {
		return nil, err
	}
	if t.Artist.ID == 0 {
		return nil, source.Fail(source.SeverityCommon, Name, "track has no artist", nil)
	}
	var page trackPage
	if err := d.api(ctx, "/artist/"+strconv.FormatInt(t.Artist.ID, 10)+"/radio", url.Values{"limit": {strconv.Itoa(radioMaxLimit)}}, &page); err != nil {
		return nil, err
	}
	return playableOnly(page.Data, ""), nil
}

func withoutSeed(tracks []source.Track, seed string) []source.Track {
	out := make([]source.Track, 0, len(tracks))
	seen := map[string]bool{seed: true}
	for _, t := range tracks {
		if seen[t.Identifier] {
			continue
		}
		seen[t.Identifier] = true
		out = append(out, t)
	}
	return out
}

func capTracks(tracks []source.Track, limit int) []source.Track {
	if len(tracks) > limit {
		return tracks[:limit]
	}
	return tracks
}
