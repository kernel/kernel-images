package pagerecovery

import (
	"math/rand/v2"
	"net/url"
	"sync"
	"time"
)

// Defaults chosen against what a caller is waiting on rather than against what
// a site might eventually allow. A Playwright goto typically carries a 30s
// timeout, and the retries have to fit inside it with the real page load still
// to come, so the budget buys a couple of short waits and then gets out of the
// way. A block that needs longer than this is a block the caller should see.
const (
	DefaultMaxAttempts = 2
	DefaultBudget      = 8 * time.Second
	defaultBaseBackoff = 300 * time.Millisecond
	// attemptTTL releases a URL's budget once a navigation is long over, so a
	// page revisited later starts fresh instead of inheriting a spent budget.
	attemptTTL = 2 * time.Minute
	// maxTrackedAttempts bounds the ledger against a session that navigates
	// through many throttled URLs without ever coming back to them.
	maxTrackedAttempts = 512
)

// Config tunes the retry budget. Zero values take the defaults.
type Config struct {
	MaxAttempts int
	Budget      time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.Budget <= 0 {
		c.Budget = DefaultBudget
	}
	return c
}

type attemptKey struct {
	session string
	url     string
}

type attemptState struct {
	attempts  int
	startedAt time.Time
	lastSeen  time.Time
}

// ledger records what a navigation has already spent. It is keyed by CDP
// session and URL rather than by request ID because each replay is a new
// request: the whole point is to recognise the URL we just retried.
type ledger struct {
	cfg Config
	now func() time.Time

	mu      sync.Mutex
	entries map[attemptKey]*attemptState
}

func newLedger(cfg Config, now func() time.Time) *ledger {
	if now == nil {
		now = time.Now
	}
	return &ledger{cfg: cfg.withDefaults(), now: now, entries: make(map[attemptKey]*attemptState)}
}

// canonical strips the fragment so a link that only differs after the hash
// shares one budget with the document it names. Query stays: it usually names a
// different page.
func canonical(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

// reserve claims one retry for a URL and reports how long to wait first.
// It reports false once the attempt count or the wall-clock budget is spent, or
// when the site asked for longer than the budget has left — waiting past the
// budget would turn a visible throttle into a navigation that looks hung.
func (l *ledger) reserve(session, rawURL string, requested time.Duration, hasRequested bool) (time.Duration, bool) {
	key := attemptKey{session: session, url: canonical(rawURL)}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictLocked(now)

	state, ok := l.entries[key]
	if !ok {
		state = &attemptState{startedAt: now}
		l.entries[key] = state
	}
	state.lastSeen = now

	if state.attempts >= l.cfg.MaxAttempts {
		return 0, false
	}
	remaining := l.cfg.Budget - now.Sub(state.startedAt)
	if remaining <= 0 {
		return 0, false
	}

	wait := backoff(state.attempts)
	if hasRequested && requested > wait {
		wait = requested
	}
	if wait > remaining {
		return 0, false
	}
	state.attempts++
	return wait, true
}

// settle releases a URL's budget once it answered with something we did not
// retry, so the next navigation to it is judged on its own. It reports how many
// retries that answer took, which is what tells a recovery apart from a page
// that was never blocked.
func (l *ledger) settle(session, rawURL string) int {
	key := attemptKey{session: session, url: canonical(rawURL)}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.entries[key]
	if !ok {
		return 0
	}
	delete(l.entries, key)
	return state.attempts
}

// forget drops a detached session's entries.
func (l *ledger) forget(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.entries {
		if key.session == session {
			delete(l.entries, key)
		}
	}
}

func (l *ledger) evictLocked(now time.Time) {
	if len(l.entries) < maxTrackedAttempts {
		for key, state := range l.entries {
			if now.Sub(state.lastSeen) > attemptTTL {
				delete(l.entries, key)
			}
		}
		return
	}
	// Over the cap, age alone is not enough: drop the least recently seen too.
	oldestKey, oldest := attemptKey{}, now
	for key, state := range l.entries {
		if now.Sub(state.lastSeen) > attemptTTL {
			delete(l.entries, key)
			continue
		}
		if !state.lastSeen.After(oldest) {
			oldestKey, oldest = key, state.lastSeen
		}
	}
	if len(l.entries) >= maxTrackedAttempts {
		delete(l.entries, oldestKey)
	}
}

// backoff is exponential with full jitter. The jitter matters more than the
// growth: many browser VMs retrying one throttled origin in lockstep is how a
// short block becomes a long one.
func backoff(attempt int) time.Duration {
	window := defaultBaseBackoff << attempt
	return window/2 + time.Duration(rand.Int64N(int64(window/2)+1))
}
