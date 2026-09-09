package agentproxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiWrapperUsesConfiguredNodeWithoutPATH(t *testing.T) {
	wrapper, err := filepath.Abs("../../runtime/acp/pi/pi-command")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatal("Pi wrapper is not executable")
	}
	dir := t.TempDir()
	node := filepath.Join(dir, "configured-node")
	output := filepath.Join(dir, "args")
	if err = os.WriteFile(node, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OUTPUT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper, "--mode", "rpc")
	command.Env = []string{"PATH=", "KERNEL_PI_NODE=" + node, "OUTPUT=" + output}
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("wrapper failed: %v: %s", err, data)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(wrapper), "pi-command.mjs") + "\n--mode\nrpc\n"
	if string(data) != want {
		t.Fatalf("arguments changed: %s", strings.TrimSpace(string(data)))
	}
}
