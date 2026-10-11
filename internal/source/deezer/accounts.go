package deezer

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

type Rotation string

const (
	RotationFailover   Rotation = "failover"
	RotationRoundRobin Rotation = "round-robin"
	RotationRandom     Rotation = "random"
)

const accountBench = 15 * time.Minute

func ParseRotation(raw string) (Rotation, error) {
	switch r := Rotation(strings.ToLower(strings.TrimSpace(raw))); r {
	case "":
		return RotationFailover, nil
	case RotationFailover, RotationRoundRobin, RotationRandom:
		return r, nil
	}
	return "", fmt.Errorf("DEEZER_ARL_ROTATION must be %s, %s or %s, got %q", RotationFailover, RotationRoundRobin, RotationRandom, raw)
}

type account struct {
	number       int
	arl          string
	mu           sync.Mutex
	session      *session
	benchedUntil time.Time
}

func (a *account) String() string {
	return fmt.Sprintf("account %d", a.number)
}

func (a *account) LogValue() slog.Value {
	return slog.IntValue(a.number)
}

func (a *account) invalidate() {
	a.mu.Lock()
	a.session = nil
	a.mu.Unlock()
}

type accountPool struct {
	mu       sync.Mutex
	accounts []*account
	rotation Rotation
	next     int
	intn     func(int) int
	now      func() time.Time
}

func newAccountPool(arls []string, rotation Rotation) *accountPool {
	p := &accountPool{rotation: rotation, intn: rand.IntN, now: time.Now}
	seen := map[string]bool{}
	for _, arl := range arls {
		if arl = strings.TrimSpace(arl); arl == "" || seen[arl] {
			continue
		}
		seen[arl] = true
		p.accounts = append(p.accounts, &account{number: len(p.accounts) + 1, arl: arl})
	}
	return p
}

func (p *accountPool) empty() bool {
	return len(p.accounts) == 0
}

func (p *accountPool) order() []*account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var healthy []*account
	for _, a := range p.accounts {
		if !now.Before(a.benchedUntil) {
			healthy = append(healthy, a)
		}
	}
	if len(healthy) < 2 {
		return healthy
	}
	start := 0
	switch p.rotation {
	case RotationRoundRobin:
		start = p.next % len(healthy)
		p.next++
	case RotationRandom:
		start = p.intn(len(healthy))
	}
	return append(healthy[start:len(healthy):len(healthy)], healthy[:start]...)
}

func (p *accountPool) bench(a *account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !now.Before(a.benchedUntil) && !p.otherHealthy(a, now) {
		return false
	}
	fresh := !now.Before(a.benchedUntil)
	a.benchedUntil = now.Add(accountBench)
	return fresh
}

func (p *accountPool) otherHealthy(a *account, now time.Time) bool {
	for _, other := range p.accounts {
		if other != a && !now.Before(other.benchedUntil) {
			return true
		}
	}
	return false
}
