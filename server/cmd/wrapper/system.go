package main

import (
	"os"
	"os/exec"
)

const scaleToZeroFile = "/uk/libukp/scale_to_zero_disable"

func disableScaleToZero() { writeScaleToZero("+") }
func enableScaleToZero()  { writeScaleToZero("-") }

func writeScaleToZero(c string) {
	if _, err := os.Stat(scaleToZeroFile); err != nil {
		return // not running on Unikraft Cloud
	}
	_ = os.WriteFile(scaleToZeroFile, []byte(c), 0o644)
}

func prepareUserDirs(asRoot bool) {
	if asRoot {
		for _, d := range []string{"/tmp", "/var/log", supervisordLogD, "/home/kernel", "/home/kernel/user-data"} {
			_ = os.MkdirAll(d, 0o755)
		}
		return
	}
	dirs := []string{
		"/home/kernel/user-data",
		"/home/kernel/.config/chromium",
		"/home/kernel/.pki/nssdb",
		"/home/kernel/.cache/dconf",
		"/tmp",
		"/var/log",
		supervisordLogD,
	}
	for _, d := range dirs {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = exec.Command("chown", "-R", "kernel:kernel",
		"/home/kernel", "/home/kernel/user-data", "/home/kernel/.config",
		"/home/kernel/.pki", "/home/kernel/.cache").Run()
	_ = exec.Command("chown", "-R", "kernel:kernel", "/etc/chromium/policies").Run()
}
