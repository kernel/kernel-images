package main

import (
	"os"
	"strings"
)

// normalizeChromiumFlags reverses the backslash-escaping that ukp-platform
// 0.10.0 applies when serializing env vars into the guest kernel cmdline: it
// prefixes special chars with a backslash that the guest cmdline parser fails
// to strip back out, so CHROMIUM_FLAGS arrives with literal backslashes glued
// to its tokens (e.g. `--no-sandbox\ --disable-gpu`). Chromium then sees
// `--no-sandbox\`, doesn't recognize it, runs with the sandbox enabled, and
// aborts with "No usable sandbox". We undo it generically (\X -> X) so the
// exact set of escaped chars doesn't matter — the result is identical to what
// a correctly-encoding host produces. No-op when there's no backslash, so it's
// safe on platform versions that encode correctly.
func normalizeChromiumFlags() {
	v := os.Getenv("CHROMIUM_FLAGS")
	if !strings.Contains(v, `\`) {
		return
	}
	var b strings.Builder
	b.Grow(len(v))
	escaped := false
	for _, r := range v {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteByte('\\')
	}
	_ = os.Setenv("CHROMIUM_FLAGS", b.String())
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
