package browserlocation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	zoneinfoDir = "/usr/share/zoneinfo"
	// localtimeCopyDir holds a regular-file copy of each zone the browser is
	// set to. ICU names the host timezone after the path /etc/localtime
	// resolves to, from "/zoneinfo/" on, and tzdata makes many identifiers
	// links to other zones (UTC to Etc/UTC, Europe/Bratislava to
	// Europe/Prague). Pointing /etc/localtime at a copy whose path ends in the
	// requested identifier makes Chromium report exactly that identifier.
	localtimeCopyDir = "/run/kernel-location/zoneinfo"
	localtimePath    = "/etc/localtime"
)

// ZonePath returns the image's zoneinfo file for timezone, rejecting
// identifiers that would resolve outside the zoneinfo directory.
func ZonePath(timezone string) (string, error) {
	return zonePath(zoneinfoDir, timezone)
}

// SetLocaltime points /etc/localtime at timezone so that Chromium, which
// watches the file, reports HostTimeZone(timezone) as its host timezone.
func SetLocaltime(timezone string) error {
	path := localtimePath
	if os.Geteuid() != 0 {
		path = "/run/kernel-location/localtime"
	}
	return setLocaltime(zoneinfoDir, localtimeCopyDir, path, timezone)
}

// HostTimeZone is the identifier the browser reports as its host timezone
// for timezone. ICU treats the identifier GMT as its built-in fixed zone,
// which pages then report as "+00:00" rather than "UTC", so GMT is hosted as
// Etc/GMT, the zone tzdata links it to.
func HostTimeZone(timezone string) string {
	if timezone == "GMT" {
		return "Etc/GMT"
	}
	return timezone
}

func zonePath(root, timezone string) (string, error) {
	if timezone == "" || filepath.IsAbs(timezone) || strings.Contains(timezone, "..") || strings.ContainsRune(timezone, '\x00') {
		return "", fmt.Errorf("invalid browser_location timezone")
	}
	path := filepath.Join(root, filepath.FromSlash(timezone))
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid browser_location timezone")
	}
	return path, nil
}

func setLocaltime(zoneinfo, copies, localtime, timezone string) error {
	source, err := zonePath(zoneinfo, timezone)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read zoneinfo: %w", err)
	}
	target, err := zonePath(copies, HostTimeZone(timezone))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create zoneinfo copy directory: %w", err)
	}
	if err := writeFileAtomic(target, data); err != nil {
		return fmt.Errorf("copy zoneinfo: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(localtime), fmt.Sprintf(".localtime-kernel-%d", time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("stage localtime: %w", err)
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, localtime); err != nil {
		return fmt.Errorf("replace localtime: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".zone-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
