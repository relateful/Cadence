package applemusic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/httpx"
)

const (
	homeURL      = "https://music.apple.com/browse"
	origin       = "https://music.apple.com"
	expiryMargin = 6 * time.Hour
)

var (
	jwtPattern    = regexp.MustCompile(`"(eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})"`)
	scriptPattern = regexp.MustCompile(`(?:src|href)="(/assets/[^"]+\.js)"`)
)

type tokenSource struct {
	http    *httpx.Client
	fixed   string
	mu      sync.Mutex
	token   string
	expires time.Time
}

func DecodeExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func FindToken(body string) string {
	if m := jwtPattern.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func ScriptURLs(html string) []string {
	var out []string
	for _, m := range scriptPattern.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(m[1], "legacy") {
			out = append(out, origin+m[1])
		}
	}
	return out
}

func (t *tokenSource) get(ctx context.Context, force bool) (string, error) {
	if t.fixed != "" {
		return t.fixed, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && t.token != "" && time.Now().Add(expiryMargin).Before(t.expires) {
		return t.token, nil
	}
	token, err := t.scrape(ctx)
	if err != nil {
		if t.token != "" {
			return t.token, nil
		}
		return "", err
	}
	exp, ok := DecodeExpiry(token)
	if !ok {
		exp = time.Now().Add(24 * time.Hour)
	}
	t.token, t.expires = token, exp
	return token, nil
}

func (t *tokenSource) scrape(ctx context.Context) (string, error) {
	html, err := t.http.Get(ctx, homeURL, nil)
	if err != nil {
		return "", err
	}
	if tok := FindToken(string(html)); tok != "" {
		return tok, nil
	}
	for _, u := range ScriptURLs(string(html)) {
		script, err := t.http.Get(ctx, u, nil)
		if err != nil {
			continue
		}
		if tok := FindToken(string(script)); tok != "" {
			return tok, nil
		}
	}
	return "", errTokenMissing
}
