package deezer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

func numbers(accounts []*account) []int {
	out := make([]int, len(accounts))
	for i, a := range accounts {
		out[i] = a.number
	}
	return out
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseRotation(t *testing.T) {
	cases := map[string]Rotation{
		"":             RotationFailover,
		"failover":     RotationFailover,
		" Round-Robin": RotationRoundRobin,
		"RANDOM":       RotationRandom,
	}
	for raw, want := range cases {
		got, err := ParseRotation(raw)
		if err != nil || got != want {
			t.Fatalf("ParseRotation(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseRotation("sticky"); err == nil {
		t.Fatal("an unknown rotation must be rejected so a typo cannot silently change behaviour")
	}
}

func TestPoolDropsBlanksAndDuplicates(t *testing.T) {
	p := newAccountPool([]string{" a ", "", "b", "a", "c"}, RotationFailover)
	if got := numbers(p.order()); !equal(got, []int{1, 2, 3}) {
		t.Fatalf("order = %v, want three distinct accounts", got)
	}
	if !newAccountPool([]string{" ", ""}, RotationFailover).empty() {
		t.Fatal("blank entries must not count as accounts")
	}
}

func TestFailoverAlwaysStartsWithTheFirstHealthyAccount(t *testing.T) {
	p := newAccountPool([]string{"a", "b", "c"}, RotationFailover)
	for range 3 {
		if got := numbers(p.order()); !equal(got, []int{1, 2, 3}) {
			t.Fatalf("order = %v, want [1 2 3] every time", got)
		}
	}
}

func TestRoundRobinRotatesTheStartAndKeepsTheRestAsFallbacks(t *testing.T) {
	p := newAccountPool([]string{"a", "b", "c"}, RotationRoundRobin)
	want := [][]int{{1, 2, 3}, {2, 3, 1}, {3, 1, 2}, {1, 2, 3}}
	for i, w := range want {
		if got := numbers(p.order()); !equal(got, w) {
			t.Fatalf("call %d: order = %v, want %v", i, got, w)
		}
	}
}

func TestRandomStartsWhereTheGeneratorSaysAndKeepsEveryAccount(t *testing.T) {
	p := newAccountPool([]string{"a", "b", "c"}, RotationRandom)
	p.intn = func(int) int { return 2 }
	if got := numbers(p.order()); !equal(got, []int{3, 1, 2}) {
		t.Fatalf("order = %v, want [3 1 2]", got)
	}
}

func TestBenchedAccountsSitOutUntilTheBenchExpires(t *testing.T) {
	now := time.Unix(0, 0)
	p := newAccountPool([]string{"a", "b"}, RotationFailover)
	p.now = func() time.Time { return now }
	if !p.bench(p.accounts[0]) {
		t.Fatal("the first bench must report itself as fresh so it is logged once")
	}
	if p.bench(p.accounts[0]) {
		t.Fatal("benching an already benched account must not report it as fresh")
	}
	if got := numbers(p.order()); !equal(got, []int{2}) {
		t.Fatalf("order = %v, want only the healthy account", got)
	}
	now = now.Add(accountBench)
	if got := numbers(p.order()); !equal(got, []int{1, 2}) {
		t.Fatalf("order = %v, want the account back after its bench", got)
	}
}

func testDeezer(arls []string, rotation Rotation) *Deezer {
	return New(nil, arls, rotation, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func rejected() error {
	return source.Fail(source.SeverityFault, Name, rejectedMessage, errARLRejected)
}

func TestFirstAccountSkipsARejectedAccountAndBenchesIt(t *testing.T) {
	d := testDeezer([]string{"dead", "live"}, RotationFailover)
	var tried []int
	u, err := firstAccount(context.Background(), d, func(a *account) (mediaLink, error) {
		tried = append(tried, a.number)
		if a.arl == "dead" {
			return mediaLink{}, rejected()
		}
		return mediaLink{URL: "ok"}, nil
	})
	if err != nil || u.URL != "ok" {
		t.Fatalf("got %+v, %v; want the live account's stream", u, err)
	}
	if !equal(tried, []int{1, 2}) {
		t.Fatalf("tried %v, want [1 2]", tried)
	}
	if got := numbers(d.accounts.order()); !equal(got, []int{2}) {
		t.Fatalf("order after rejection = %v, want the dead account benched", got)
	}
}

func TestFirstAccountTriesAnotherAccountWhenATrackIsRefused(t *testing.T) {
	d := testDeezer([]string{"free", "premium"}, RotationFailover)
	u, err := firstAccount(context.Background(), d, func(a *account) (mediaLink, error) {
		if a.arl == "free" {
			return mediaLink{}, source.Fail(source.SeveritySuspicious, Name, "media refused", errors.New("2002"))
		}
		return mediaLink{URL: "ok"}, nil
	})
	if err != nil || u.URL != "ok" {
		t.Fatalf("got %+v, %v; want the second account's stream", u, err)
	}
	if got := numbers(d.accounts.order()); !equal(got, []int{1, 2}) {
		t.Fatalf("a refused track must not bench the account, order = %v", got)
	}
}

func TestFirstAccountStopsOnAnErrorAnotherAccountCannotFix(t *testing.T) {
	d := testDeezer([]string{"a", "b"}, RotationFailover)
	calls := 0
	_, err := firstAccount(context.Background(), d, func(*account) (mediaLink, error) {
		calls++
		return mediaLink{}, source.Fail(source.SeverityCommon, Name, "track has no stream token", nil)
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want one call and the error returned", calls, err)
	}
}

func TestFirstAccountReturnsTheRejectionWhenEveryAccountFails(t *testing.T) {
	d := testDeezer([]string{"a", "b"}, RotationFailover)
	_, err := firstAccount(context.Background(), d, func(*account) (mediaLink, error) { return mediaLink{}, rejected() })
	if !isRejected(err) {
		t.Fatalf("err = %v, want an arl rejected failure", err)
	}
}

func TestAnAccountNeverPrintsItsARL(t *testing.T) {
	p := newAccountPool([]string{"secret-arl-value"}, RotationFailover)
	a := p.accounts[0]
	for _, s := range []string{fmt.Sprint(a), fmt.Sprintf("%v", a), fmt.Sprintf("%+v", a), fmt.Sprintf("%s", a)} {
		if strings.Contains(s, "secret") {
			t.Fatalf("formatted account leaked its ARL: %q", s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "account", a)
	if strings.Contains(buf.String(), "secret") || !strings.Contains(buf.String(), `"account":1`) {
		t.Fatalf("logged account = %s, want only its number", buf.String())
	}
}

func TestFirstAccountMovesOnWhenAnAccountIsRateLimited(t *testing.T) {
	d := testDeezer([]string{"limited", "spare"}, RotationFailover)
	u, err := firstAccount(context.Background(), d, func(a *account) (mediaLink, error) {
		if a.arl == "limited" {
			return mediaLink{}, &httpx.StatusError{Status: 429}
		}
		return mediaLink{URL: "ok"}, nil
	})
	if err != nil || u.URL != "ok" {
		t.Fatalf("got %+v, %v; want the spare account's stream", u, err)
	}
}

func TestTheLastWorkingAccountIsNeverBenched(t *testing.T) {
	single := testDeezer([]string{"only"}, RotationFailover)
	for range 2 {
		_, err := firstAccount(context.Background(), single, func(*account) (mediaLink, error) { return mediaLink{}, rejected() })
		if !isRejected(err) {
			t.Fatalf("err = %v, want the rejection itself", err)
		}
	}
	if got := numbers(single.accounts.order()); !equal(got, []int{1}) {
		t.Fatalf("order = %v, a lone account must stay in rotation so the next request asks Deezer again", got)
	}
	pair := testDeezer([]string{"a", "b"}, RotationFailover)
	_, _ = firstAccount(context.Background(), pair, func(*account) (mediaLink, error) { return mediaLink{}, rejected() })
	if got := numbers(pair.accounts.order()); !equal(got, []int{2}) {
		t.Fatalf("order = %v, want the first benched and the last kept", got)
	}
}
