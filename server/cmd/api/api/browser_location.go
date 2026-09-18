package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"golang.org/x/text/language"
)

const browserLocationApplyTimeout = 5 * time.Second

type browserLocationBundle struct {
	Epoch      string   `json:"epoch"`
	Generation uint64   `json:"generation"`
	TimeZone   string   `json:"timezone"`
	Locale     string   `json:"locale"`
	Languages  []string `json:"languages"`
}

type browserLocationStatus struct {
	Accepted *browserLocationBundle `json:"accepted,omitempty"`
	Applied  *browserLocationBundle `json:"applied,omitempty"`
	Error    string                 `json:"error,omitempty"`
}

var (
	errStaleBrowserLocation    = errors.New("browser location generation is stale")
	errConflictBrowserLocation = errors.New("browser location generation conflicts with accepted payload")
)

func validateBrowserLocationBundle(raw string) (browserLocationBundle, error) {
	var bundle browserLocationBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return bundle, fmt.Errorf("invalid browser_location JSON")
	}
	if bundle.Epoch == "" || bundle.Generation == 0 {
		return bundle, fmt.Errorf("browser_location epoch and generation are required")
	}
	zonePath, err := browserLocationZonePath(bundle.TimeZone)
	if err != nil {
		return bundle, err
	}
	if _, err := os.Stat(zonePath); err != nil {
		return bundle, fmt.Errorf("unsupported browser_location timezone")
	}
	locale, err := language.Parse(bundle.Locale)
	if err != nil || locale.String() != bundle.Locale {
		return bundle, fmt.Errorf("browser_location locale must be canonical BCP 47")
	}
	if len(bundle.Languages) == 0 || bundle.Languages[0] != bundle.Locale {
		return bundle, fmt.Errorf("browser_location languages must start with locale")
	}
	for _, value := range bundle.Languages {
		tag, err := language.Parse(value)
		if err != nil || tag.String() != value {
			return bundle, fmt.Errorf("browser_location languages must be canonical BCP 47")
		}
	}
	return bundle, nil
}

func browserLocationZonePath(timezone string) (string, error) {
	if timezone == "" || filepath.IsAbs(timezone) || strings.Contains(timezone, "..") || strings.ContainsRune(timezone, '\x00') {
		return "", fmt.Errorf("invalid browser_location timezone")
	}
	root := "/usr/share/zoneinfo"
	path := filepath.Join(root, filepath.FromSlash(timezone))
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid browser_location timezone")
	}
	return path, nil
}

func (s *ApiService) acceptBrowserLocation(bundle browserLocationBundle) error {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	current := s.browserLocation.accepted
	if current != nil && current.Epoch == bundle.Epoch {
		if bundle.Generation < current.Generation {
			return errStaleBrowserLocation
		}
		if bundle.Generation == current.Generation && !browserLocationBundlesEqual(*current, bundle) {
			return errConflictBrowserLocation
		}
	}
	copy := bundle
	s.browserLocation.accepted = &copy
	s.browserLocation.lastError = ""
	if s.browserLocation.cancel != nil {
		s.browserLocation.cancel()
	}
	ctx, cancel := context.WithCancel(s.lifecycleCtx)
	s.browserLocation.cancel = cancel
	go s.reconcileBrowserLocation(ctx, copy)
	return nil
}

func (s *ApiService) reconcileBrowserLocation(ctx context.Context, bundle browserLocationBundle) {
	deadline := time.Now().Add(browserLocationApplyTimeout)
	var lastErr error
	for {
		if err := rewriteLocaltime(bundle.TimeZone); err != nil {
			lastErr = err
		} else {
			lastErr = s.withCDPClientTimeout(ctx, time.Second, func(cdpCtx context.Context, client *cdpclient.Client) error {
				languages := strings.Join(bundle.Languages, ",")
				if err := client.SetBrowserLocation(cdpCtx, bundle.Locale, languages); err != nil {
					return err
				}
				observed, err := client.GetBrowserLocation(cdpCtx)
				if err != nil {
					return err
				}
				if observed.Locale != bundle.Locale || observed.AcceptLanguages != languages || observed.TimeZone != bundle.TimeZone {
					return fmt.Errorf("browser location has not converged")
				}
				return nil
			})
		}
		if lastErr == nil {
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				copy := bundle
				s.browserLocation.applied = &copy
				s.browserLocation.lastError = ""
			}
			s.browserLocationMu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				s.browserLocation.lastError = lastErr.Error()
			}
			s.browserLocationMu.Unlock()
			logger.FromContext(ctx).Error("browser location did not converge", "error", lastErr, "generation", bundle.Generation)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func rewriteLocaltime(timezone string) error {
	target, err := browserLocationZonePath(timezone)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("/etc/.localtime-kernel-%d", time.Now().UnixNano())
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("stage localtime: %w", err)
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, "/etc/localtime"); err != nil {
		return fmt.Errorf("replace localtime: %w", err)
	}
	return nil
}

func (s *ApiService) browserLocationSnapshot() browserLocationStatus {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	return browserLocationStatus{Accepted: s.browserLocation.accepted, Applied: s.browserLocation.applied, Error: s.browserLocation.lastError}
}

// GetBrowserLocationHTTP exposes accepted/applied state for internal reconciliation.
func (s *ApiService) GetBrowserLocationHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.browserLocationSnapshot())
}

func browserLocationBundlesEqual(a, b browserLocationBundle) bool {
	return a.Epoch == b.Epoch && a.Generation == b.Generation && a.TimeZone == b.TimeZone && a.Locale == b.Locale && slices.Equal(a.Languages, b.Languages)
}
