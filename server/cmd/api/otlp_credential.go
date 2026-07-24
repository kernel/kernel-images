package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

// otlpIdentityCacheTTL bounds how often the provider re-reads the applied
// fork-identity payload from disk. Export requests are frequent; the identity
// changes at most once per fork, so a few seconds of staleness is harmless and
// avoids a filesystem read on every request. Only a payload-sourced identity is
// cached; a boot fallback during the pre-apply window is returned uncached so
// the fresh identity is picked up on the very next request.
const otlpIdentityCacheTTL = 3 * time.Second

// otlpIdentity is the platform identity stamped onto OTLP export: the relay
// bearer credential plus the resource attributes.
type otlpIdentity struct {
	jwt          string
	instanceName string
	metro        string
}

// otlpIdentityProvider resolves the OTLP identity per use.
//
// In fork-identity-wait mode the platform delivers a fresh KERNEL_INSTANCE_JWT
// (and INST_NAME/METRO_NAME) in the applied identity payload after
// kernel-images-api has already started, and the process is not restarted, so
// the boot-time env values are stale (empty on an unbound fork). This provider
// reads the identity from the applied payload instead, falling back to the boot
// values. Outside fork mode it returns the boot identity unchanged.
type otlpIdentityProvider struct {
	boot        otlpIdentity
	forkEnabled bool

	mu       sync.Mutex
	cached   otlpIdentity
	cachedOK bool
	cachedAt time.Time
}

func newOTLPIdentityProvider(boot otlpIdentity, log *slog.Logger) *otlpIdentityProvider {
	enabled, err := forkidentity.WaitEnabled()
	if err != nil {
		// Malformed flag: treat as disabled but surface it, otherwise a fork VM
		// would silently keep using the stale boot identity.
		log.Warn("fork-identity wait flag invalid; treating as disabled", "err", err)
	}
	return &otlpIdentityProvider{boot: boot, forkEnabled: enabled}
}

func (p *otlpIdentityProvider) Token() string        { return p.current().jwt }
func (p *otlpIdentityProvider) InstanceName() string { return p.current().instanceName }
func (p *otlpIdentityProvider) Metro() string        { return p.current().metro }

// current returns the identity, caching only a payload-sourced (or non-fork,
// stable) result so a boot fallback during the pre-apply window is retried on
// the next call rather than pinned for the TTL.
func (p *otlpIdentityProvider) current() otlpIdentity {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedOK && time.Since(p.cachedAt) < otlpIdentityCacheTTL {
		return p.cached
	}
	id, ok := p.resolve()
	if ok {
		p.cached, p.cachedOK, p.cachedAt = id, true, time.Now()
	}
	return id
}

// resolve reads the applied fork-identity payload. The bool reports whether the
// result is stable/cacheable: true for the boot identity outside fork mode or a
// successfully read payload, false for a boot fallback while a fork is mid-apply.
func (p *otlpIdentityProvider) resolve() (otlpIdentity, bool) {
	if !p.forkEnabled {
		return p.boot, true
	}
	// Only read once the applied marker is present, to avoid a torn read while
	// the wrapper is mid-apply.
	applied, err := forkidentity.ReadAppliedMarker()
	if err != nil || applied == "" {
		return p.boot, false
	}
	payload, err := forkidentity.ReadPayload()
	if err != nil {
		return p.boot, false
	}
	return otlpIdentity{
		jwt:          forkidentity.FirstNonEmpty(payload.Get("kernel_instance_jwt"), p.boot.jwt),
		instanceName: forkidentity.FirstNonEmpty(payload.InstanceName(), p.boot.instanceName),
		metro:        forkidentity.FirstNonEmpty(payload.Get("metro_name"), p.boot.metro),
	}, true
}
