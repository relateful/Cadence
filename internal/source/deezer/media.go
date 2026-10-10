package deezer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/source"
)

const (
	gwBase         = "https://www.deezer.com/ajax/gw-light.php"
	mediaURL       = "https://media.deezer.com/v1/get_url"
	sessionTTL     = time.Hour
	mediaURLTTL    = 10 * time.Minute
	streamCipher   = "BF_CBC_STRIPE"
	formatMP3High  = "MP3_128"
	formatMP3Low   = "MP3_64"
	gwInvalidToken = "VALID_TOKEN_REQUIRED"
)

var ErrNoARL = errors.New("DEEZER_ARL is not set, so full-length streams are unavailable")

type session struct {
	apiToken     string
	licenseToken string
	sid          string
	expires      time.Time
}

type sessionStore struct {
	mu      sync.Mutex
	current *session
}

type gwResponse struct {
	Error   json.RawMessage `json:"error"`
	Results json.RawMessage `json:"results"`
}

func gwFailed(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "[]" || trimmed == "{}" || trimmed == "null" {
		return "", false
	}
	return trimmed, true
}

func (d *Deezer) gw(ctx context.Context, s *session, method string, body any, out any) error {
	q := url.Values{"method": {method}, "input": {"3"}, "api_version": {"1.0"}, "api_token": {""}}
	if s != nil {
		q.Set("api_token", s.apiToken)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, gwBase+"?"+q.Encode(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setEnglish(req)
	cookie := "arl=" + d.arl
	if s != nil && s.sid != "" {
		cookie += "; sid=" + s.sid
	}
	req.Header.Set("Cookie", cookie)
	res, err := d.http.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var gr gwResponse
	if err := json.NewDecoder(res.Body).Decode(&gr); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if msg, failed := gwFailed(gr.Error); failed {
		if strings.Contains(msg, gwInvalidToken) {
			d.sessions.invalidate()
		}
		return source.Fail(source.SeveritySuspicious, Name, method+" refused", errors.New(msg))
	}
	if s == nil {
		for _, c := range res.Cookies() {
			if c.Name == "sid" {
				out.(*userData).sid = c.Value
			}
		}
	}
	return json.Unmarshal(gr.Results, out)
}

type userData struct {
	CheckForm string `json:"checkForm"`
	User      struct {
		ID      int64 `json:"USER_ID"`
		Options struct {
			LicenseToken string `json:"license_token"`
		} `json:"OPTIONS"`
	} `json:"USER"`
	sid string
}

func (s *sessionStore) invalidate() {
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
}

func (d *Deezer) session(ctx context.Context) (*session, error) {
	if d.arl == "" {
		return nil, source.Fail(source.SeverityFault, Name, "stream unavailable", ErrNoARL)
	}
	d.sessions.mu.Lock()
	defer d.sessions.mu.Unlock()
	if s := d.sessions.current; s != nil && time.Now().Before(s.expires) {
		return s, nil
	}
	var u userData
	if err := d.gw(ctx, nil, "deezer.getUserData", map[string]any{}, &u); err != nil {
		return nil, err
	}
	if u.User.ID == 0 || u.User.Options.LicenseToken == "" {
		return nil, source.Fail(source.SeverityFault, Name, "arl rejected", errors.New("the ARL is expired or the account was logged out; replace DEEZER_ARL"))
	}
	s := &session{apiToken: u.CheckForm, licenseToken: u.User.Options.LicenseToken, sid: u.sid, expires: time.Now().Add(sessionTTL)}
	d.sessions.current = s
	return s, nil
}

type songData struct {
	Data []struct {
		TrackToken string `json:"TRACK_TOKEN"`
		Fallback   *struct {
			ID string `json:"SNG_ID"`
		} `json:"FALLBACK"`
	} `json:"data"`
}

type mediaResponse struct {
	Data []struct {
		Media []struct {
			Format  string `json:"format"`
			Sources []struct {
				URL string `json:"url"`
			} `json:"sources"`
		} `json:"media"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"data"`
}

type mediaFormat struct{ Cipher, Format string }

func (d *Deezer) mediaURL(ctx context.Context, trackID string) (string, error) {
	if u, ok := d.urls.Get(trackID); ok {
		return u, nil
	}
	u, err := d.fetchMediaURL(ctx, trackID)
	if err != nil {
		var f *source.Failure
		if errors.As(err, &f) && f.Severity == source.SeveritySuspicious {
			d.sessions.invalidate()
			u, err = d.fetchMediaURL(ctx, trackID)
		}
	}
	if err != nil {
		return "", err
	}
	d.urls.Set(trackID, u)
	return u, nil
}

func (d *Deezer) fetchMediaURL(ctx context.Context, trackID string) (string, error) {
	s, err := d.session(ctx)
	if err != nil {
		return "", err
	}
	var songs songData
	if err := d.gw(ctx, s, "song.getListData", map[string]any{"sng_ids": []string{trackID}}, &songs); err != nil {
		return "", err
	}
	if len(songs.Data) == 0 || songs.Data[0].TrackToken == "" {
		return "", source.Fail(source.SeverityCommon, Name, "track has no stream token", nil)
	}
	formats := make([]map[string]string, 0, len(d.formats))
	for _, f := range d.formats {
		formats = append(formats, map[string]string{"cipher": f.Cipher, "format": f.Format})
	}
	payload, _ := json.Marshal(map[string]any{
		"license_token": s.licenseToken,
		"media":         []map[string]any{{"type": "FULL", "formats": formats}},
		"track_tokens":  []string{songs.Data[0].TrackToken},
	})
	req, err := http.NewRequest(http.MethodPost, mediaURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	setEnglish(req)
	body, err := d.http.Send(ctx, req)
	if err != nil {
		return "", err
	}
	var mr mediaResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return "", err
	}
	if len(mr.Data) == 0 {
		return "", source.Fail(source.SeverityCommon, Name, "no media returned", nil)
	}
	if errs := mr.Data[0].Errors; len(errs) > 0 {
		return "", source.Fail(source.SeveritySuspicious, Name, "media refused", fmt.Errorf("%d: %s", errs[0].Code, errs[0].Message))
	}
	for _, m := range mr.Data[0].Media {
		for _, src := range m.Sources {
			if src.URL != "" {
				return src.URL, nil
			}
		}
	}
	return "", source.Fail(source.SeverityCommon, Name, "no stream url returned", nil)
}

func setEnglish(req *http.Request) {
	for k, v := range englishHeaders {
		req.Header.Set(k, v)
	}
}
