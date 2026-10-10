package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ProxyCooldown  = time.Minute
	requestTimeout = 15 * time.Second
	headerTimeout  = 6 * time.Second
	idleTimeout    = 30 * time.Second
	pingAfter      = 15 * time.Second
	pingTimeout    = 5 * time.Second
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
)

type proxyState struct {
	url          *url.URL
	blockedUntil atomic.Int64
}

func (p *proxyState) usable(now time.Time) bool {
	return p != nil && p.url != nil && now.UnixNano() >= p.blockedUntil.Load()
}

type Client struct {
	name   string
	proxy  *proxyState
	viaPx  *http.Client
	direct *http.Client
	log    *slog.Logger
}

func transport(proxy *url.URL) *http.Transport {
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       idleTimeout,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: pingAfter, PingTimeout: pingTimeout},
	}
	if proxy != nil {
		t.Proxy = http.ProxyURL(proxy)
	}
	return t
}

func New(name, proxyURL string, log *slog.Logger) (*Client, error) {
	c := &Client{name: name, direct: &http.Client{Transport: transport(nil)}, log: log.With("http", name)}
	if proxyURL == "" {
		return c, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("%s proxy: %w", name, err)
	}
	c.proxy = &proxyState{url: u}
	c.viaPx = &http.Client{Transport: transport(u)}
	return c, nil
}

func (c *Client) markProxyFailed(err error) {
	if !c.proxy.usable(time.Now()) {
		return
	}
	c.proxy.blockedUntil.Store(time.Now().Add(ProxyCooldown).UnixNano())
	c.log.Warn("proxy failed, using direct", "error", err, "cooldownSeconds", int(ProxyCooldown.Seconds()))
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if c.proxy.usable(time.Now()) {
		res, err := c.viaPx.Do(req)
		if err == nil {
			return res, nil
		}
		if req.Context().Err() != nil || (req.Body != nil && req.GetBody == nil) {
			return nil, scrub(err)
		}
		c.markProxyFailed(scrub(err))
		if req.GetBody != nil {
			body, berr := req.GetBody()
			if berr != nil {
				return nil, berr
			}
			req.Body = body
		}
	}
	res, err := c.direct.Do(req)
	if err != nil && retryable(req, err) {
		c.log.Warn("request failed on a pooled connection, retrying on a fresh one", "error", scrub(err))
		c.direct.CloseIdleConnections()
		if req.GetBody != nil {
			body, berr := req.GetBody()
			if berr != nil {
				return nil, berr
			}
			req.Body = body
		}
		res, err = c.direct.Do(req)
	}
	return res, scrub(err)
}

func retryable(req *http.Request, err error) bool {
	if req.Context().Err() != nil {
		return false
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection") ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

type StatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.URL, e.Status, e.Body)
}

func IsStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.read(req)
}

func (c *Client) Send(ctx context.Context, req *http.Request) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return c.read(req.WithContext(ctx))
}

func (c *Client) read(req *http.Request) ([]byte, error) {
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, &StatusError{Status: res.StatusCode, URL: SafeURL(req.URL), Body: snippet}
	}
	return body, nil
}

func (c *Client) Stream(ctx context.Context, req *http.Request) (*http.Response, error) {
	res, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		res.Body.Close()
		return nil, &StatusError{Status: res.StatusCode, URL: SafeURL(req.URL)}
	}
	return res, nil
}

func SafeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clean := *u
	clean.User = nil
	if clean.RawQuery != "" {
		clean.RawQuery = "redacted"
	}
	clean.Fragment = ""
	return clean.String()
}

func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if parsed, perr := url.Parse(ue.URL); perr == nil {
			ue.URL = SafeURL(parsed)
		} else {
			ue.URL = "(unparseable url)"
		}
	}
	return err
}
