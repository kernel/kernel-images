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

	"github.com/kernel/kernel-images/server/lib/statefile"
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
	if err := statefile.WriteJSON(path, persisted{Enabled: enabled}); err != nil {
		return fmt.Errorf("persist webauthn hook state: %w", err)
	}
	return nil
}
