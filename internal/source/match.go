package source

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

const (
	MatchDriftMs   = 5000
	minMatchScore  = 0.55
	versionPenalty = 0.4
)

var versionMarkers = []string{"remix", "live", "cover", "karaoke", "instrumental", "acoustic", "sped up", "slowed", "nightcore", "8d", "edit", "remaster", "demo", "reverb"}

func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.Fields(normalize(s)) {
		out[f] = true
	}
	return out
}

func overlap(a, b map[string]bool) float64 {
	if len(a) == 0 {
		return 0
	}
	hit := 0
	for t := range a {
		if b[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(a))
}

func markers(s string) map[string]bool {
	n := " " + normalize(s) + " "
	out := map[string]bool{}
	for _, m := range versionMarkers {
		if strings.Contains(n, " "+m+" ") {
			out[m] = true
		}
	}
	return out
}

func WithinDrift(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	return math.Abs(float64(a-b)) <= MatchDriftMs
}

func MatchScore(target, candidate Track) float64 {
	if target.ISRC != "" && NormalizeISRC(target.ISRC) == NormalizeISRC(candidate.ISRC) {
		if WithinDrift(target.DurationMs, candidate.DurationMs) {
			return 2
		}
		return 0
	}
	if !WithinDrift(target.DurationMs, candidate.DurationMs) {
		return 0
	}
	score := 0.65*overlap(tokens(target.Title), tokens(candidate.Title)) + 0.35*overlap(tokens(target.Author), tokens(candidate.Author+" "+candidate.Title))
	want, got := markers(target.Title), markers(candidate.Title)
	for m := range got {
		if !want[m] {
			score -= versionPenalty
		}
	}
	for m := range want {
		if !got[m] {
			score -= versionPenalty
		}
	}
	return score
}

func BestMatch(target Track, candidates []Track) (Track, bool) {
	best, bestScore := Track{}, minMatchScore
	found := false
	for _, c := range candidates {
		if s := MatchScore(target, c); s >= bestScore {
			best, bestScore, found = c, s, true
		}
	}
	return best, found
}

func unwantedMarkers(query string, t Track) int {
	want, n := markers(query), 0
	for m := range markers(t.Title) {
		if !want[m] {
			n++
		}
	}
	return n
}

func RankForQuery(query string, tracks []Track) []Track {
	out := append([]Track(nil), tracks...)
	sort.SliceStable(out, func(i, j int) bool {
		return unwantedMarkers(query, out[i]) < unwantedMarkers(query, out[j])
	})
	return out
}
