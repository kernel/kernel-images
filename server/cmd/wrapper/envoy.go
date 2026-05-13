package main

import "os"

// envoyEnabled mirrors init-envoy.sh's gate: when any of these are unset
// the script exits early without starting envoy, so we should skip the
// readiness probe too (otherwise it would just time out at 60s).
func envoyEnabled() bool {
	return os.Getenv("INST_NAME") != "" &&
		os.Getenv("METRO_NAME") != "" &&
		os.Getenv("XDS_SERVER") != "" &&
		os.Getenv("KERNEL_INSTANCE_JWT") != ""
}
