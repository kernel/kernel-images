package main

import (
	"os"
	"strings"
)

// unescapeCmdline drops a backslash that escapes the following byte (`\X` -> `X`),
// scanning left to right so a doubled backslash collapses to one (`\\` -> `\`) and
// a lone trailing backslash is preserved.
func unescapeCmdline(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// normalizeChromiumFlags reverses the backslash-escaping the platform's cmdline
// encoder inserts before special chars and the guest parser fails to strip:
// CHROMIUM_FLAGS arrives as e.g. `--no-sandbox\ --disable-gpu`, so Chromium
// sees `--no-sandbox\`, ignores it, runs with the sandbox enabled, and aborts
// with "No usable sandbox". Reversing every `\X` -> `X` is safe because the
// flag list never contains a real backslash; a no-op when none are present.
func normalizeChromiumFlags() {
	if v := os.Getenv("CHROMIUM_FLAGS"); strings.Contains(v, `\`) {
		_ = os.Setenv("CHROMIUM_FLAGS", unescapeCmdline(v))
	}
}

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
