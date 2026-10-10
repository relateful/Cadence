package httpx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestErrorsNeverCarryQueryStrings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := New("test", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := c.Get(context.Background(), srv.URL+"/tracks?client_id=SECRETCLIENTID&sig=SIGNED", nil)
	if err == nil || strings.Contains(err.Error(), "SECRETCLIENTID") || strings.Contains(err.Error(), "SIGNED") {
		t.Fatalf("status error leaked the query: %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.URL
	dead.Close()
	_, err = c.Get(context.Background(), addr+"/x?client_id=SECRETCLIENTID", nil)
	if err == nil || strings.Contains(err.Error(), "SECRETCLIENTID") {
		t.Fatalf("transport error leaked the query: %v", err)
	}
}

func TestSafeURLKeepsHostAndPath(t *testing.T) {
	u, _ := url.Parse("https://user:pw@api.example.com/v1/tracks/5?client_id=abc#frag")
	if got := SafeURL(u); got != "https://api.example.com/v1/tracks/5?redacted" {
		t.Fatalf("got %s", got)
	}
}
