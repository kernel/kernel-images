package main

import (
	"os"
	"strings"
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

