package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

// identityCacheTTL bounds how often the provider re-reads the applied
// fork-identity payload from disk. Export requests are frequent; the identity
// changes at most once per fork, so a few seconds of staleness is harmless and
// avoids a filesystem read on every request. Only a payload-sourced identity is
// cached; a boot fallback during the pre-apply window is returned uncached so
// the fresh identity is picked up on the very next request.
const identityCacheTTL = 3 * time.Second

// instanceIdentity is the platform identity this VM presents to Kernel
// services: the instance JWT used as a bearer credential (by the OTLP relay
// and the metro API), the OTLP resource attributes, and the metro API URL.
type instanceIdentity struct {
	jwt          string
	instanceName string
	metro        string
	metroAPIURL  string
}

// instanceIdentityProvider resolves the instance identity per use.
//
// In fork-identity-wait mode the platform delivers a fresh KERNEL_INSTANCE_JWT
// (and INST_NAME/METRO_NAME/SESSION_INTEL_URL) in the applied identity payload
// after kernel-images-api has already started, and the process is not
// restarted, so the boot-time env values are stale (empty on an unbound fork).
// This provider reads the identity from the applied payload instead, falling
// back to the boot values. Outside fork mode it returns the boot identity
// unchanged.
type instanceIdentityProvider struct {
	boot        instanceIdentity
	forkEnabled bool

	mu       sync.Mutex
	cached   instanceIdentity
	cachedOK bool
	cachedAt time.Time
}

func newInstanceIdentityProvider(boot instanceIdentity, log *slog.Logger) *instanceIdentityProvider {
	enabled, err := forkidentity.WaitEnabled()
	if err != nil {
		// Malformed flag: treat as disabled but surface it, otherwise a fork VM
		// would silently keep using the stale boot identity.
		log.Warn("fork-identity wait flag invalid; treating as disabled", "err", err)
	}
	return &instanceIdentityProvider{boot: boot, forkEnabled: enabled}
}

func (p *instanceIdentityProvider) Token() string        { return p.current().jwt }
func (p *instanceIdentityProvider) InstanceName() string { return p.current().instanceName }
func (p *instanceIdentityProvider) Metro() string        { return p.current().metro }
func (p *instanceIdentityProvider) MetroAPIURL() string  { return p.current().metroAPIURL }

// current returns the identity, caching only a payload-sourced (or non-fork,
// stable) result so a boot fallback during the pre-apply window is retried on
// the next call rather than pinned for the TTL.
func (p *instanceIdentityProvider) current() instanceIdentity {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedOK && time.Since(p.cachedAt) < identityCacheTTL {
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
func (p *instanceIdentityProvider) resolve() (instanceIdentity, bool) {
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
	return instanceIdentity{
		jwt:          forkidentity.FirstNonEmpty(payload.Get("kernel_instance_jwt"), p.boot.jwt),
		instanceName: forkidentity.FirstNonEmpty(payload.InstanceName(), p.boot.instanceName),
		metro:        forkidentity.FirstNonEmpty(payload.Get("metro_name"), p.boot.metro),
		metroAPIURL:  forkidentity.FirstNonEmpty(forkidentity.ExtensionAPIURL(payload), p.boot.metroAPIURL),
	}, true
}
