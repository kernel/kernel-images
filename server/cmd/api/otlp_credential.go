package main

import (
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

// instanceJWTCacheTTL bounds how often the provider re-reads the applied
// fork-identity payload from disk. Export requests are frequent; the JWT changes
// at most once per fork, so a few seconds of staleness is harmless and avoids a
// filesystem read on every request.
const instanceJWTCacheTTL = 3 * time.Second

// instanceJWTProvider resolves the OTLP relay bearer credential per request.
//
// In fork-identity-wait mode the platform delivers a fresh KERNEL_INSTANCE_JWT
// in the applied identity payload after kernel-images-api has already started,
// and the process is not restarted, so the boot-time env JWT is stale (empty on
// an unbound fork). This provider reads the JWT from the applied payload instead,
// falling back to the boot env otherwise. Outside fork mode it returns the boot
// JWT unchanged.
type instanceJWTProvider struct {
	bootJWT     string
	forkEnabled bool

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

func newInstanceJWTProvider(bootJWT string) *instanceJWTProvider {
	enabled, _ := forkidentity.WaitEnabled()
	return &instanceJWTProvider{bootJWT: bootJWT, forkEnabled: enabled}
}

// Token returns the current bearer credential, or "" when none is available yet
// (the caller then sends no Authorization header rather than an empty bearer).
func (p *instanceJWTProvider) Token() string {
	if !p.forkEnabled {
		return p.bootJWT
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < instanceJWTCacheTTL {
		return p.cached
	}
	p.cached = p.resolve()
	p.cachedAt = time.Now()
	return p.cached
}

// resolve reads the applied fork-identity payload for the JWT. It only reads once
// the applied marker is present, to avoid a torn read while the wrapper is
// mid-apply, and falls back to the boot JWT on any miss.
func (p *instanceJWTProvider) resolve() string {
	applied, err := forkidentity.ReadAppliedMarker()
	if err != nil || applied == "" {
		return p.bootJWT
	}
	payload, err := forkidentity.ReadPayload()
	if err != nil {
		return p.bootJWT
	}
	if jwt := payload.Get("kernel_instance_jwt"); jwt != "" {
		return jwt
	}
	return p.bootJWT
}
