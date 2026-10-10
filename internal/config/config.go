package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const insecurePassword = "youshallnotpass"

type Config struct {
	Address           string
	Password          string
	BufferMs          int
	PlayoutDelay      time.Duration
	Bitrate           int
	SearchOrder       []string
	ResumeTimeout     time.Duration
	UpdateInterval    time.Duration
	AnalysisWorkers   int
	AnalysisQueue     int
	LogLevel          string
	DeezerARL         string
	DeezerHighQuality bool
	DeezerMasterKey   string
	DeezerProxy       string
	SpotifyID         string
	SpotifySecret     string
	SpotifyProxy      string
	AppleToken        string
	AppleStorefront   string
	AppleProxy        string
}

type Lookup func(string) (string, bool)

func str(get Lookup, key, fallback string) string {
	if v, ok := get(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func num(get Lookup, key string, fallback int) (int, error) {
	raw := str(get, key, "")
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func flag(get Lookup, key string, fallback bool) bool {
	switch strings.ToLower(str(get, key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

func Load(get Lookup) (Config, error) {
	c := Config{
		Address:           str(get, "CADENCE_ADDRESS", "0.0.0.0:2333"),
		Password:          str(get, "CADENCE_PASSWORD", ""),
		LogLevel:          str(get, "CADENCE_LOG_LEVEL", "info"),
		DeezerARL:         str(get, "DEEZER_ARL", ""),
		DeezerHighQuality: flag(get, "DEEZER_HIGH_QUALITY", true),
		DeezerMasterKey:   str(get, "DEEZER_MASTER_KEY", ""),
		DeezerProxy:       str(get, "DEEZER_PROXY", ""),
		SpotifyID:         str(get, "SPOTIFY_CLIENT_ID", ""),
		SpotifySecret:     str(get, "SPOTIFY_CLIENT_SECRET", ""),
		SpotifyProxy:      str(get, "SPOTIFY_PROXY", ""),
		AppleToken:        str(get, "APPLE_MUSIC_TOKEN", ""),
		AppleStorefront:   str(get, "APPLE_MUSIC_STOREFRONT", "us"),
		AppleProxy:        str(get, "APPLE_MUSIC_PROXY", ""),
	}
	if c.Password == "" || c.Password == insecurePassword {
		return Config{}, errors.New("CADENCE_PASSWORD must be set to a non-default value")
	}
	ints := []struct {
		key      string
		fallback int
		target   *int
	}{
		{"CADENCE_BUFFER_MS", 10_000, &c.BufferMs},
		{"CADENCE_BITRATE", 96_000, &c.Bitrate},
		{"CADENCE_ANALYSIS_WORKERS", 2, &c.AnalysisWorkers},
		{"CADENCE_ANALYSIS_QUEUE", 32, &c.AnalysisQueue},
	}
	for _, i := range ints {
		v, err := num(get, i.key, i.fallback)
		if err != nil {
			return Config{}, err
		}
		*i.target = v
	}
	durations := []struct {
		key      string
		fallback int
		unit     time.Duration
		target   *time.Duration
	}{
		{"CADENCE_PLAYOUT_DELAY_MS", 0, time.Millisecond, &c.PlayoutDelay},
		{"CADENCE_RESUME_TIMEOUT_SECONDS", 300, time.Second, &c.ResumeTimeout},
		{"CADENCE_UPDATE_INTERVAL_MS", 1000, time.Millisecond, &c.UpdateInterval},
	}
	for _, d := range durations {
		v, err := num(get, d.key, d.fallback)
		if err != nil {
			return Config{}, err
		}
		*d.target = time.Duration(v) * d.unit
	}
	c.BufferMs = max(1000, min(60_000, c.BufferMs))
	c.UpdateInterval = max(250*time.Millisecond, c.UpdateInterval)
	for _, s := range strings.Split(str(get, "CADENCE_SEARCH_ORDER", "applemusic,spotify,deezer"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.SearchOrder = append(c.SearchOrder, s)
		}
	}
	return c, nil
}

func FromEnv() (Config, error) {
	return Load(os.LookupEnv)
}

func (c Config) Warnings() []string {
	var out []string
	if c.DeezerARL == "" {
		out = append(out, "DEEZER_ARL is not set: search works, full-length playback does not")
	}
	if c.DeezerMasterKey == "" {
		out = append(out, "DEEZER_MASTER_KEY is not set: Deezer streams cannot be decrypted, so nothing will play")
	}
	if c.SpotifyID == "" || c.SpotifySecret == "" {
		out = append(out, "SPOTIFY_CLIENT_ID/SECRET are not set: Spotify links will not resolve")
	}
	return out
}
