package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/kernel-images/server/lib/chromiumflags"
	"github.com/kernel/kernel-images/server/lib/fillfence"
	"github.com/kernel/kernel-images/server/lib/x11"
)

// shared/start-pulseaudio.sh is the authority for the audio topology. These are
// the fixed contract values it creates, not overridable defaults: chromium must
// connect to the same socket and play into the same sink the daemon sets up.
// Keep them in sync with start-pulseaudio.sh.
const (
	// pulseServer is the PulseAudio socket the recorder and chromium share.
	pulseServer = "unix:/tmp/pulse/native"
	// pulseSink is the null sink chromium plays into; the recorder captures
	// its .monitor source.
	pulseSink = "KernelOutput"

	defaultPrivateNetworkBypassFlag = "--proxy-bypass-list=10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7"
)

func main() {
	// Pdeathsig is tied to the creating thread. Keep the browser's parent thread
	// alive for the owner's entire lifetime.
	runtime.LockOSThread()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	headless := flag.Bool("headless", false, "Run Chromium with headless flags")
	chromiumPath := flag.String("chromium", "chromium", "Chromium binary path (default: chromium)")
	runtimeFlagsPath := flag.String("runtime-flags", "/chromium/flags", "Path to runtime flags overlay file")
	flag.Parse()

	// Inputs
	internalPort := strings.TrimSpace(os.Getenv("INTERNAL_PORT"))
	if internalPort == "" {
		internalPort = "9223"
	}

	// Wait for the X server. The wrapper starts chromium in parallel with
	// xorg/xvfb, so the display socket may not be ready yet — without this
	// gate chromium would fail on connect and supervisord would restart us.
	if d := x11.WaitForDisplay(":1", 20*time.Second); d >= 20*time.Second {
		fmt.Fprintf(os.Stderr, "warning: X display :1 not responsive after %s\n", d)
	}

	// Headful: wait for mutter to register before exec'ing chromium. If
	// chromium maps its window with no WM present, the CSD hint it sends has
	// no listener; mutter starts later, reparents the existing window, and
	// applies default SSD — i.e., the titlebar with the close X. Headless
	// has no WM, so skip.
	if !*headless {
		if d := x11.WaitForMutter(20 * time.Second); d >= 20*time.Second {
			fmt.Fprintf(os.Stderr, "warning: mutter not registered after %s\n", d)
		}
	}

	baseFlags := os.Getenv("CHROMIUM_FLAGS")
	runtimeTokens, err := chromiumflags.ReadOptionalFlagFile(*runtimeFlagsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed reading runtime flags: %v\n", err)
		os.Exit(1)
	}
	final := chromiumflags.MergeFlagsWithRuntimeTokens(baseFlags, runtimeTokens)
	final = chromiumflags.TranslateKernelDisableFeatures(final)
	final = withDefaultPrivateNetworkBypass(final)

	// Diagnostics for parity with previous scripts
	fmt.Printf("BASE_FLAGS: %s\n", baseFlags)
	fmt.Printf("RUNTIME_FLAGS: %s\n", strings.Join(runtimeTokens, " "))
	fmt.Printf("FINAL_FLAGS: %s\n", strings.Join(final, " "))

	// flags we send no matter what
	chromiumArgs := []string{
		fmt.Sprintf("--remote-debugging-port=%s", internalPort),
		"--remote-allow-origins=*",
		"--user-data-dir=/home/kernel/user-data",
		"--password-store=basic",
		"--no-first-run",
	}
	if *headless {
		chromiumArgs = append([]string{"--headless=new"}, chromiumArgs...)
	}
	chromiumArgs = append(chromiumArgs, final...)

	runAsRoot := strings.EqualFold(strings.TrimSpace(os.Getenv("RUN_AS_ROOT")), "true")

	// Prepare environment. PULSE_SERVER/PULSE_SINK route chromium's audio into the
	// recorder's sink, for both root and kernel-user launches.
	env := os.Environ()
	env = append(env,
		"DISPLAY=:1",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket",
		"PULSE_SERVER="+pulseServer,
		"PULSE_SINK="+pulseSink,
	)

	path, err := execLookPath(*chromiumPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chromium binary not found: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(path, chromiumArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if !runAsRoot {
		credential, err := kernelCredential()
		if err != nil {
			fmt.Fprintf(os.Stderr, "kernel user unavailable: %v\n", err)
			os.Exit(1)
		}
		cmd.SysProcAttr.Credential = credential
		env = append(env, "USER=kernel", "LOGNAME=kernel", "HOME=/home/kernel", "XDG_CONFIG_HOME=/home/kernel/.config", "XDG_CACHE_HOME=/home/kernel/.cache")
	}
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	browser := fillfence.Browser{Command: cmd, Executable: cmd.Path, ProfileDir: "/home/kernel/user-data", DevToolsPort: internalPort, Address: fillfence.Address, Identity: fillfence.Identity}
	if err := browser.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "browser owner stopped: %v\n", err)
		os.Exit(1)
	}
}

func withDefaultPrivateNetworkBypass(flags []string) []string {
	for _, flag := range flags {
		if flag == "--proxy-bypass-list" || strings.HasPrefix(flag, "--proxy-bypass-list=") {
			return flags
		}
	}
	return append(flags, defaultPrivateNetworkBypassFlag)
}

func execLookPath(file string) (string, error) {
	if strings.ContainsRune(file, os.PathSeparator) {
		return file, nil
	}
	return exec.LookPath(file)
}

func kernelCredential() (*syscall.Credential, error) {
	u, err := user.Lookup("kernel")
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	groups, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	credential := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	for _, group := range groups {
		id, err := strconv.ParseUint(group, 10, 32)
		if err != nil {
			return nil, err
		}
		credential.Groups = append(credential.Groups, uint32(id))
	}
	return credential, nil
}
