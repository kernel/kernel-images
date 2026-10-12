// The on/off switch set by PUT /webauthn/hook is written to
// /run/kernel/webauthn-hook.json. If the kernel-images API process restarts,
// it reads the file and turns the hook back on, so browsers with armed
// passkeys keep working. /run is per boot, so a new VM always starts with the
// hook off. A file that cannot be read means off.

package webauthnhook

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// DefaultStatePath sits with the other per-boot state the control plane
// hands the VM.
const DefaultStatePath = "/run/kernel/webauthn-hook.json"

type persisted struct {
	Enabled bool `json:"enabled"`
}

// loadEnabled reads the flag a previous run left. Anything unreadable leaves
// the hook off, which is the browser's own behavior.
func loadEnabled(path string, log *slog.Logger) bool {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	var p persisted
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err != nil {
		log.Error("webauthn: could not read the hook state; leaving the hook off", "path", path, "err", err)
		return false
	}
	return p.Enabled
}

func saveEnabled(path string, enabled bool) error {
	data, err := json.Marshal(persisted{Enabled: enabled})
	if err != nil {
		return fmt.Errorf("encode webauthn hook state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create webauthn hook state directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write webauthn hook state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace webauthn hook state: %w", err)
	}
	return nil
}
