// Package statefile writes the small JSON files that hold state the control
// plane hands the VM, so it survives a restart of the API process.
package statefile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteJSON replaces path with v encoded as JSON, creating its directory. It
// writes through a rename, so a reader after a crash sees the old value or
// the new one, never a torn file.
func WriteJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}
