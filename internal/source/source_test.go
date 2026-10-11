package source

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
)

type fakeAudio struct {
	isrc     map[string]Track
	results  []Track
	searches atomic.Int32
}

func (f *fakeAudio) Name() string                                        { return "fake" }
func (f *fakeAudio) SearchPrefix() string                                { return "fksearch:" }
func (f *fakeAudio) MatchesURL(string) bool                              { return false }
func (f *fakeAudio) LoadURL(context.Context, string) (LoadResult, error) { return Empty(), nil }
func (f *fakeAudio) Track(context.Context, string) (Track, error)        { return Track{}, errors.New("no") }
func (f *fakeAudio) Open(context.Context, Track) (Stream, error)         { return Stream{}, nil }
func (f *fakeAudio) Warm(context.Context, Track)                         {}

func (f *fakeAudio) Search(_ context.Context, _ string) ([]Track, error) {
	f.searches.Add(1)
	return f.results, nil
}

func (f *fakeAudio) ByISRC(_ context.Context, isrc string) (Track, error) {
	if t, ok := f.isrc[isrc]; ok {
		return t, nil
	}
	return Track{}, Fail(SeverityCommon, "fake", "not found", nil)
}

type fakeMirror struct {
	full Track
}

func (m fakeMirror) Name() string                                        { return "mirror" }
func (m fakeMirror) SearchPrefix() string                                { return "mrsearch:" }
func (m fakeMirror) MatchesURL(u string) bool                            { return u == "https://mirror/track" }
func (m fakeMirror) LoadURL(context.Context, string) (LoadResult, error) { return Single(m.full), nil }
func (m fakeMirror) Search(context.Context, string) ([]Track, error)     { return []Track{m.full}, nil }
func (m fakeMirror) Track(context.Context, string) (Track, error)        { return m.full, nil }

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTrackEncodingRoundTrips(t *testing.T) {
	in := Track{Source: "deezer", Identifier: "3135553", Title: "One More Time", Author: "Daft Punk", DurationMs: 320000, ISRC: "GBDUW0000053"}
	out, err := DecodeTrack(in.Encode())
	if err != nil || out != in {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := DecodeTrack("not base64!"); err == nil {
		t.Fatal("garbage decoded")
	}
	if _, err := DecodeTrack(Track{Title: "x"}.Encode()); err == nil {
		t.Fatal("track without source decoded")
	}
}

func TestMatchPrefersISRCAndRejectsWrongVersion(t *testing.T) {
	target := Track{Title: "Blinding Lights", Author: "The Weeknd", DurationMs: 200000, ISRC: "USUG11904206"}
	candidates := []Track{
		{Identifier: "remix", Title: "Blinding Lights (Remix)", Author: "The Weeknd", DurationMs: 201000},
		{Identifier: "live", Title: "Blinding Lights - Live", Author: "The Weeknd", DurationMs: 199000},
		{Identifier: "orig", Title: "Blinding Lights", Author: "The Weeknd", DurationMs: 200500},
	}
	best, ok := BestMatch(target, candidates)
	if !ok || best.Identifier != "orig" {
		t.Fatalf("picked %+v", best)
	}
	isrcHit := Track{Identifier: "isrc", Title: "Totally different", DurationMs: 200000, ISRC: "US-UG1-19-04206"}
	if best, _ := BestMatch(target, append(candidates, isrcHit)); best.Identifier != "isrc" {
		t.Fatalf("isrc not preferred: %+v", best)
	}
	if _, ok := BestMatch(target, []Track{{Title: "Blinding Lights", Author: "The Weeknd", DurationMs: 260000}}); ok {
		t.Fatal("accepted a 60s duration drift")
	}
	sped := Track{Title: "Blinding Lights (Sped Up)", Author: "The Weeknd", DurationMs: 170000}
	if _, ok := BestMatch(Track{Title: "Blinding Lights (Sped Up)", Author: "The Weeknd", DurationMs: 170000}, []Track{sped}); !ok {
		t.Fatal("rejected an explicitly requested version")
	}
}

func TestResolverMirrorsByISRCAndCaches(t *testing.T) {
	hit := Track{Source: "fake", Identifier: "42", Title: "Song", Author: "Artist", DurationMs: 180000, ISRC: "GBAAA0000001"}
	audio := &fakeAudio{isrc: map[string]Track{"GBAAA0000001": hit}}
	mirror := fakeMirror{full: Track{Source: "mirror", Identifier: "m1", Title: "Song", Author: "Artist", DurationMs: 181000, ISRC: "GB-AAA-00-00001"}}
	r := NewResolver(audio, []string{"mirror", "fake"}, logger(), mirror)
	partial := Track{Source: "mirror", Identifier: "m1", Title: "Song", Author: "Artist"}
	got, err := r.Playable(context.Background(), partial)
	if err != nil || got.Identifier != "42" {
		t.Fatalf("%+v %v", got, err)
	}
	r.Playable(context.Background(), partial)
	if audio.searches.Load() != 0 {
		t.Fatal("text search used despite an ISRC hit")
	}
}

func TestResolverFallsBackToTextSearchAndCachesMisses(t *testing.T) {
	audio := &fakeAudio{results: []Track{{Source: "fake", Identifier: "7", Title: "Other Song", Author: "Nobody", DurationMs: 100000}}}
	r := NewResolver(audio, nil, logger())
	target := Track{Source: "mirror", Identifier: "x", Title: "Song", Author: "Artist", DurationMs: 180000}
	if _, err := r.Playable(context.Background(), target); err == nil {
		t.Fatal("matched an unrelated song")
	}
	r.Playable(context.Background(), target)
	if audio.searches.Load() != 1 {
		t.Fatalf("miss not cached: %d searches", audio.searches.Load())
	}
}

func TestResolverRoutesIdentifiers(t *testing.T) {
	hit := Track{Source: "fake", Identifier: "42", Title: "Song", DurationMs: 1000}
	audio := &fakeAudio{isrc: map[string]Track{"GBAAA0000001": hit}, results: []Track{hit}}
	mirror := fakeMirror{full: Track{Source: "mirror", Identifier: "m1", Title: "Song"}}
	r := NewResolver(audio, []string{"mirror", "fake"}, logger(), mirror)
	ctx := context.Background()
	if res := r.Load(ctx, "dzisrc:GB-AAA-00-00001"); res.Type != LoadTrack || res.Tracks[0].Identifier != "42" {
		t.Fatalf("isrc %+v", res)
	}
	if res := r.Load(ctx, "https://mirror/track"); res.Type != LoadTrack || res.Tracks[0].Source != "mirror" {
		t.Fatalf("url %+v", res)
	}
	if res := r.Load(ctx, "song"); res.Type != LoadSearch || res.Tracks[0].Source != "mirror" {
		t.Fatalf("search order %+v", res)
	}
	if res := r.Load(ctx, "fksearch:song"); res.Tracks[0].Source != "fake" {
		t.Fatalf("prefix %+v", res)
	}
	if res := r.Load(ctx, "https://unknown.example/x"); res.Type != LoadError {
		t.Fatalf("unknown url %+v", res)
	}
}

func TestEnqueueIsBoundedAndDeduplicated(t *testing.T) {
	r := NewResolver(&fakeAudio{}, nil, logger())
	r.mu.Lock()
	for i := 0; i < prefetchPending; i++ {
		r.warming[string(rune('a'+i%26))+string(rune('0'+i/26))] = struct{}{}
	}
	r.mu.Unlock()
	if r.Enqueue(Track{Source: "fake", Identifier: "new"}) {
		t.Fatal("enqueue accepted past the pending bound")
	}
	r.mu.Lock()
	r.warming = map[string]struct{}{"fake:dup": {}}
	r.mu.Unlock()
	if !r.Enqueue(Track{Source: "fake", Identifier: "dup"}) {
		t.Fatal("duplicate of a pending warm was refused")
	}
	r.mu.Lock()
	n := len(r.warming)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("duplicate started a second warm (%d pending)", n)
	}
}

func TestYouTubeSearchPrefixesSearchTheAudioSource(t *testing.T) {
	audio := &fakeAudio{results: []Track{{Source: "fake", Identifier: "1", Title: "Song"}}}
	r := NewResolver(audio, nil, logger())
	for _, q := range []string{"ytmsearch:song", "ytsearch: song"} {
		res := r.Load(context.Background(), q)
		if res.Type != LoadSearch || res.Tracks[0].Source != "fake" {
			t.Fatalf("%s: %+v", q, res)
		}
	}
}

func TestSearchDemotesVersionsTheQueryDidNotAskFor(t *testing.T) {
	in := []Track{
		{Identifier: "k", Title: "Let The Light In (Instrumental Karaoke Version)"},
		{Identifier: "o", Title: "Let The Light In"},
		{Identifier: "l", Title: "Let The Light In (Live)"},
		{Identifier: "o2", Title: "Let The Light In (feat. Father John Misty)"},
	}
	got := RankForQuery("let the light in", in)
	order := ""
	for _, t := range got {
		order += t.Identifier + ","
	}
	if order != "o,o2,l,k," {
		t.Fatalf("order %s", order)
	}
	if RankForQuery("let the light in karaoke", in)[0].Identifier != "k" {
		t.Fatal("asking for karaoke should keep karaoke first")
	}
}

type fakeFallback struct {
	results []Track
	opened  atomic.Int32
}

func (f *fakeFallback) Name() string                                        { return "fb" }
func (f *fakeFallback) SearchPrefix() string                                { return "fbsearch:" }
func (f *fakeFallback) MatchesURL(string) bool                              { return false }
func (f *fakeFallback) LoadURL(context.Context, string) (LoadResult, error) { return Empty(), nil }
func (f *fakeFallback) Track(context.Context, string) (Track, error) {
	return Track{}, errors.New("no")
}
func (f *fakeFallback) Search(context.Context, string) ([]Track, error) { return f.results, nil }
func (f *fakeFallback) Warm(context.Context, Track)                     {}
func (f *fakeFallback) Open(context.Context, Track) (Stream, error) {
	f.opened.Add(1)
	return Stream{}, nil
}

func TestFallbackAnswersOnlyWhenThePrimaryHasNothing(t *testing.T) {
	fbTrack := Track{Source: "fb", Identifier: "9", Title: "Rare Song", Author: "Indie", DurationMs: 200000}
	primary := &fakeAudio{}
	fb := &fakeFallback{results: []Track{fbTrack}}
	r := NewResolver(primary, nil, logger())
	r.UseFallback(fb)

	if res := r.Load(context.Background(), "fksearch:rare song"); res.Type != LoadSearch || res.Tracks[0].Source != "fb" {
		t.Fatalf("empty primary search: %+v", res)
	}
	spotify := Track{Source: "mirror", Identifier: "s1", Title: "Rare Song", Author: "Indie", DurationMs: 201000}
	got, err := r.Playable(context.Background(), spotify)
	if err != nil || got.Source != "fb" {
		t.Fatalf("mirror to fallback: %+v %v", got, err)
	}
	if _, err := r.Open(context.Background(), got, 0); err != nil || fb.opened.Load() != 1 {
		t.Fatalf("open routed wrong: %v opened=%d", err, fb.opened.Load())
	}

	primary.results = []Track{{Source: "fake", Identifier: "1", Title: "Common Song", Author: "Band", DurationMs: 180000}}
	if res := r.Load(context.Background(), "fksearch:common song"); res.Tracks[0].Source != "fake" {
		t.Fatalf("fallback used although the primary had results: %+v", res)
	}
	mirrored, err := r.Playable(context.Background(), Track{Source: "mirror", Identifier: "s2", Title: "Common Song", Author: "Band", DurationMs: 181000})
	if err != nil || mirrored.Source != "fake" {
		t.Fatalf("primary should win the mirror: %+v %v", mirrored, err)
	}
}

type fakeRecommender struct {
	fakeAudio
	seeds []string
	out   []Track
	err   error
}

func (f *fakeRecommender) Recommend(_ context.Context, seed string, limit int) ([]Track, error) {
	f.seeds = append(f.seeds, seed)
	if limit != recommendLimit {
		return nil, errors.New("unexpected limit")
	}
	return f.out, f.err
}

func TestRecommendPrefixAsksTheAudioSource(t *testing.T) {
	audio := &fakeRecommender{out: []Track{{Source: "fake", Identifier: "2", Title: "Next"}}}
	r := NewResolver(audio, nil, logger())
	res := r.Load(context.Background(), RecommendPrefix+" 110956532 ")
	if res.Type != LoadSearch || len(res.Tracks) != 1 || res.Tracks[0].Identifier != "2" {
		t.Fatalf("got %+v, want the recommended track as search results", res)
	}
	if len(audio.seeds) != 1 || audio.seeds[0] != "110956532" {
		t.Fatalf("seeds = %q, want the trimmed seed", audio.seeds)
	}
}

func TestRecommendPrefixWithoutARecommenderOrResultsIsEmpty(t *testing.T) {
	if res := NewResolver(&fakeAudio{}, nil, logger()).Load(context.Background(), RecommendPrefix+"1"); res.Type != LoadEmpty {
		t.Fatalf("a source that cannot recommend must give an empty result, got %v", res.Type)
	}
	if res := NewResolver(&fakeRecommender{}, nil, logger()).Load(context.Background(), RecommendPrefix+"1"); res.Type != LoadEmpty {
		t.Fatalf("no recommendations must give an empty result, got %v", res.Type)
	}
	failing := &fakeRecommender{err: errors.New("boom")}
	if res := NewResolver(failing, nil, logger()).Load(context.Background(), RecommendPrefix+"1"); res.Type != LoadError {
		t.Fatalf("a failure must surface as a load error, got %v", res.Type)
	}
}

func TestFanEditsAreRecognised(t *testing.T) {
	edits := []string{"Gilded Lily (Sped Up)", "This Side of Paradise (slowed)", "i met sarah in the bathroom {slow//reverb}", "Swing Lynn (Slowed Version)", "Song - Nightcore", "Track (8D Audio)", "Hit (Karaoke Version)"}
	for _, title := range edits {
		if !FanEdit(Track{Title: title}) {
			t.Fatalf("%q should be a fan edit", title)
		}
	}
	real := []string{"Sweater Weather", "Song (Live)", "Song (Remix)", "Song - Acoustic", "Speedway", "Slowdive"}
	for _, title := range real {
		if FanEdit(Track{Title: title}) {
			t.Fatalf("%q is not a fan edit", title)
		}
	}
}

func TestRecommendationsDropFanEdits(t *testing.T) {
	audio := &fakeRecommender{out: []Track{{Identifier: "1", Title: "Gilded Lily"}, {Identifier: "2", Title: "Gilded Lily (Sped Up)"}}}
	res := NewResolver(audio, nil, logger()).Load(context.Background(), RecommendPrefix+"9")
	if len(res.Tracks) != 1 || res.Tracks[0].Identifier != "1" {
		t.Fatalf("got %+v, want only the original", res.Tracks)
	}
}
