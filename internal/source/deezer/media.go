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
	"time"

	"github.com/greed/cadence/internal/metrics"
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

const rejectedMessage = "arl rejected"

var (
	ErrNoARL       = errors.New("DEEZER_ARL is not set, so full-length streams are unavailable")
	errARLRejected = errors.New("the ARL is expired or the account was logged out; replace it in DEEZER_ARL")
)

type session struct {
	apiToken     string
	licenseToken string
	sid          string
	expires      time.Time
}

func isRejected(err error) bool {
	var f *source.Failure
	return errors.As(err, &f) && f.Message == rejectedMessage
}

func isSuspicious(err error) bool {
	return err != nil && source.Classify(Name, err).Severity == source.SeveritySuspicious
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

func (d *Deezer) gw(ctx context.Context, a *account, s *session, method string, body any, out any) error {
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
	cookie := "arl=" + a.arl
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
		if s != nil && strings.Contains(msg, gwInvalidToken) {
			a.invalidate()
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

func (d *Deezer) session(ctx context.Context, a *account) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.session; s != nil && time.Now().Before(s.expires) {
		return s, nil
	}
	var u userData
	if err := d.gw(ctx, a, nil, "deezer.getUserData", map[string]any{}, &u); err != nil {
		return nil, err
	}
	if u.User.ID == 0 || u.User.Options.LicenseToken == "" {
		return nil, source.Fail(source.SeverityFault, Name, rejectedMessage, errARLRejected)
	}
	s := &session{apiToken: u.CheckForm, licenseToken: u.User.Options.LicenseToken, sid: u.sid, expires: time.Now().Add(sessionTTL)}
	a.session = s
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

type mediaLink struct {
	URL    string
	Format string
}

func (d *Deezer) mediaURL(ctx context.Context, trackID string) (mediaLink, error) {
	if u, ok := d.urls.Get(trackID); ok {
		return u, nil
	}
	if d.accounts.empty() {
		return mediaLink{}, source.Fail(source.SeverityFault, Name, "stream unavailable", ErrNoARL)
	}
	u, err := firstAccount(ctx, d, func(a *account) (mediaLink, error) { return d.mediaURLFrom(ctx, a, trackID) })
	if err != nil {
		return mediaLink{}, err
	}
	d.urls.Set(trackID, u)
	return u, nil
}

func firstAccount[T any](ctx context.Context, d *Deezer, fetch func(*account) (T, error)) (T, error) {
	var (
		zero T
		err  error
	)
	for i, a := range d.accounts.order() {
		var u T
		if u, err = fetch(a); err == nil {
			if i > 0 {
				metrics.DeezerAccounts.Inc("switched")
			}
			return u, nil
		}
		if ctx.Err() != nil {
			break
		}
		if isRejected(err) {
			metrics.DeezerAccounts.Inc("rejected")
			if d.accounts.bench(a) {
				d.log.Error("deezer account rejected, benched", "account", a, "accounts", len(d.accounts.accounts), "for", accountBench)
			}
			continue
		}
		if !isSuspicious(err) {
			break
		}
	}
	return zero, err
}

func (d *Deezer) mediaURLFrom(ctx context.Context, a *account, trackID string) (mediaLink, error) {
	u, err := d.fetchMediaURL(ctx, a, trackID)
	if err != nil && isSuspicious(err) {
		a.invalidate()
		u, err = d.fetchMediaURL(ctx, a, trackID)
	}
	return u, err
}

func (d *Deezer) fetchMediaURL(ctx context.Context, a *account, trackID string) (mediaLink, error) {
	s, err := d.session(ctx, a)
	if err != nil {
		return mediaLink{}, err
	}
	var songs songData
	if err := d.gw(ctx, a, s, "song.getListData", map[string]any{"sng_ids": []string{trackID}}, &songs); err != nil {
		return mediaLink{}, err
	}
	if len(songs.Data) == 0 || songs.Data[0].TrackToken == "" {
		return mediaLink{}, source.Fail(source.SeverityCommon, Name, "track has no stream token", nil)
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
		return mediaLink{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setEnglish(req)
	body, err := d.http.Send(ctx, req)
	if err != nil {
		return mediaLink{}, err
	}
	var mr mediaResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return mediaLink{}, err
	}
	if len(mr.Data) == 0 {
		return mediaLink{}, source.Fail(source.SeverityCommon, Name, "no media returned", nil)
	}
	if errs := mr.Data[0].Errors; len(errs) > 0 {
		return mediaLink{}, source.Fail(source.SeveritySuspicious, Name, "media refused", fmt.Errorf("%d: %s", errs[0].Code, errs[0].Message))
	}
	for _, m := range mr.Data[0].Media {
		for _, src := range m.Sources {
			if src.URL != "" {
				return mediaLink{URL: src.URL, Format: m.Format}, nil
			}
		}
	}
	return mediaLink{}, source.Fail(source.SeverityCommon, Name, "no stream url returned", nil)
}

func setEnglish(req *http.Request) {
	for k, v := range englishHeaders {
		req.Header.Set(k, v)
	}
}
