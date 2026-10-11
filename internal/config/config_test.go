package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := values[k]
		return v, ok
	}
}

func TestPasswordIsRequired(t *testing.T) {
	if _, err := Load(env(nil)); err == nil {
		t.Fatal("empty password accepted")
	}
	if _, err := Load(env(map[string]string{"CADENCE_PASSWORD": "youshallnotpass"})); err == nil {
		t.Fatal("lavalink default password accepted")
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Address != "0.0.0.0:2333" || c.BufferMs != 10_000 || c.Bitrate != 96_000 || c.ResumeTimeout != 5*time.Minute {
		t.Fatalf("%+v", c)
	}
	if len(c.SearchOrder) != 3 || c.SearchOrder[0] != "applemusic" {
		t.Fatalf("search order %v", c.SearchOrder)
	}
}

func TestOverridesAndClamps(t *testing.T) {
	c, err := Load(env(map[string]string{
		"CADENCE_PASSWORD":               "s3cret",
		"CADENCE_BUFFER_MS":              "100",
		"CADENCE_RESUME_TIMEOUT_SECONDS": "60",
		"CADENCE_PLAYOUT_DELAY_MS":       "80",
		"CADENCE_SEARCH_ORDER":           "deezer, spotify",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BufferMs != 1000 || c.ResumeTimeout != time.Minute || c.PlayoutDelay != 80*time.Millisecond || c.SearchOrder[1] != "spotify" {
		t.Fatalf("%+v", c)
	}
	if _, err := Load(env(map[string]string{"CADENCE_PASSWORD": "x", "CADENCE_BITRATE": "fast"})); err == nil {
		t.Fatal("non-numeric bitrate accepted")
	}
}

func TestMasterKeyIsReadAndWarnedWhenMissing(t *testing.T) {
	missing, _ := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret"}))
	if !slices.ContainsFunc(missing.Warnings(), func(w string) bool { return strings.HasPrefix(w, "DEEZER_MASTER_KEY") }) {
		t.Fatalf("no master key warning in %v", missing.Warnings())
	}
	set, _ := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret", "DEEZER_MASTER_KEY": " 0123456789abcdef "}))
	if set.DeezerMasterKey != "0123456789abcdef" {
		t.Fatalf("key %q", set.DeezerMasterKey)
	}
	if slices.ContainsFunc(set.Warnings(), func(w string) bool { return strings.HasPrefix(w, "DEEZER_MASTER_KEY") }) {
		t.Fatal("warned with key set")
	}
}

func TestAudioCacheIsOffByDefaultAndClamped(t *testing.T) {
	c, _ := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret"}))
	if c.AudioCacheDir != "" || c.AudioCacheGB != 20 {
		t.Fatalf("defaults %q %d", c.AudioCacheDir, c.AudioCacheGB)
	}
	c, _ = Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret", "CADENCE_AUDIO_CACHE_DIR": "/var/cache/cadence", "CADENCE_AUDIO_CACHE_GB": "0"}))
	if c.AudioCacheDir != "/var/cache/cadence" || c.AudioCacheGB != 1 {
		t.Fatalf("set %q %d", c.AudioCacheDir, c.AudioCacheGB)
	}
}

func TestNormalizeDefaultsAndClamp(t *testing.T) {
	c, _ := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret"}))
	if !c.Normalize || c.NormalizeLufs != -14 {
		t.Fatalf("defaults %v %d", c.Normalize, c.NormalizeLufs)
	}
	c, _ = Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret", "CADENCE_NORMALIZE": "false", "CADENCE_NORMALIZE_LUFS": "0"}))
	if c.Normalize || c.NormalizeLufs != -5 {
		t.Fatalf("set %v %d", c.Normalize, c.NormalizeLufs)
	}
}

func TestDeezerARLAcceptsAList(t *testing.T) {
	c, err := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret", "DEEZER_ARL": " first , ,second", "DEEZER_ARL_ROTATION": "round-robin"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.DeezerARLs) != 2 || c.DeezerARLs[0] != "first" || c.DeezerARLs[1] != "second" {
		t.Fatalf("DeezerARLs = %q, want [first second]", c.DeezerARLs)
	}
	if c.DeezerRotation != "round-robin" {
		t.Fatalf("DeezerRotation = %q", c.DeezerRotation)
	}
	single, _ := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret", "DEEZER_ARL": "only"}))
	if len(single.DeezerARLs) != 1 || single.DeezerARLs[0] != "only" {
		t.Fatalf("a single ARL must still work unchanged, got %q", single.DeezerARLs)
	}
	for _, w := range single.Warnings() {
		if strings.HasPrefix(w, "DEEZER_ARL") {
			t.Fatalf("no ARL warning expected when one is set, got %q", w)
		}
	}
}

func TestDeezerARLOfOnlyBlanksCountsAsUnset(t *testing.T) {
	c, _ := Load(env(map[string]string{"CADENCE_PASSWORD": "s3cret", "DEEZER_ARL": " , ,"}))
	if len(c.DeezerARLs) != 0 {
		t.Fatalf("DeezerARLs = %q, want none", c.DeezerARLs)
	}
	warned := false
	for _, w := range c.Warnings() {
		warned = warned || strings.HasPrefix(w, "DEEZER_ARL")
	}
	if !warned {
		t.Fatal("a list of only blanks must still warn that no ARL is set")
	}
}
