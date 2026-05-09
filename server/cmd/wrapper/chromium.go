package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// applyHeadlessDefaultFlags mirrors the legacy headless wrapper.sh: when
// CHROMIUM_FLAGS is unset, fill in a curated headless+stealth flag list.
// --disable-background-networking is intentionally omitted: it prevents
// Chrome from fetching ExtensionInstallForcelist managed extensions.
func applyHeadlessDefaultFlags() {
	if strings.TrimSpace(os.Getenv("CHROMIUM_FLAGS")) != "" {
		return
	}
	flags := strings.Join([]string{
		"--accept-lang=en-US,en",
		"--allow-pre-commit-input",
		"--blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4",
		"--crash-dumps-dir=/tmp/chromium-dumps",
		"--disable-back-forward-cache",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-blink-features=AutomationControlled",
		"--disable-breakpad",
		"--disable-client-side-phishing-detection",
		"--disable-component-extensions-with-background-pages",
		"--disable-crash-reporter",
		"--disable-crashpad",
		"--disable-dev-shm-usage",
		"--disable-features=AcceptCHFrame,AutoExpandDetailsElement,AvoidUnnecessaryBeforeUnloadCheckSync,CertificateTransparencyComponentUpdater,DeferRendererTasksAfterInput,DestroyProfileOnBrowserClose,DialMediaRouteProvider,ExtensionManifestV2Disabled,GlobalMediaControls,HttpsUpgrades,ImprovedCookieControls,LazyFrameLoading,LensOverlay,MediaRouter,PaintHolding,ThirdPartyStoragePartitioning,Translate",
		"--disable-field-trial-config",
		"--disable-gcm-registration",
		"--disable-gpu",
		"--disable-gpu-compositing",
		"--disable-hang-monitor",
		"--disable-ipc-flooding-protection",
		"--disable-notifications",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--disable-renderer-backgrounding",
		"--disable-search-engine-choice-screen",
		"--disable-software-rasterizer",
		"--enable-use-zoom-for-dsf=false",
		"--export-tagged-pdf",
		"--force-color-profile=srgb",
		"--hide-crash-restore-bubble",
		"--hide-scrollbars",
		"--metrics-recording-only",
		"--mute-audio",
		"--no-default-browser-check",
		"--no-first-run",
		"--no-sandbox",
		"--no-service-autorun",
		"--ozone-platform=headless",
		"--password-store=basic",
		"--unsafely-disable-devtools-self-xss-warnings",
		"--use-angle=swiftshader",
		"--use-gl=angle",
		"--use-mock-keychain",
	}, " ")
	_ = os.Setenv("CHROMIUM_FLAGS", flags)
}

// dismissNoSandboxWarning replicates the wrapper.sh behaviour of clicking the
// "X" on the --no-sandbox infobar. Cosmetic; runs off the hot path.
func dismissNoSandboxWarning() {
	out, err := exec.Command("xdotool", "getdisplaygeometry").Output()
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return
	}
	width := parts[0]
	x := width
	if w, err := strconv.Atoi(width); err == nil && w > 30 {
		x = strconv.Itoa(w - 30)
	}
	target := "New Tab - Chromium"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("xdotool", "search", "--name", target).Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			id := strings.Fields(string(out))[0]
			if exec.Command("xdotool", "windowactivate", "--sync", id).Run() == nil {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Without a settle delay the click can land before the --no-sandbox infobar
	// has finished painting, leaving the warning on screen. The legacy
	// wrapper.sh slept 5s here for the same reason. Runs off the hot path
	// (goroutine fired post-readiness) so this doesn't extend time-to-CDP.
	time.Sleep(5 * time.Second)
	port := os.Getenv("KERNEL_IMAGES_API_PORT")
	if port == "" {
		port = defaultAPIPort
	}
	body := fmt.Sprintf(`{"x":%s,"y":115}`, x)
	_ = exec.Command("curl", "-s", "-o", "/dev/null", "-X", "POST",
		"http://localhost:"+port+"/computer/click_mouse",
		"-H", "Content-Type: application/json",
		"-d", body).Run()
}

