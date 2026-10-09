package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/browserlocation"
	"github.com/stretchr/testify/require"
)

// requireKernelBrowser skips tests that need Kernel's browser-location CDP
// commands, which stock Chrome for Testing images do not have.
func requireKernelBrowser(t *testing.T) {
	t.Helper()
	if os.Getenv("E2E_KERNEL_BROWSER") != "1" {
		t.Skip("set E2E_KERNEL_BROWSER=1 when the image ships a kernel-browser build")
	}
}

// capabilityLocalesScript lists the canonical locales the browser resolves
// exactly: Intl.DateTimeFormat and Intl.NumberFormat must resolve the tag
// itself, and Intl.Collator the tag or a parent obtained by removing subtags.
// Candidates are the language, script and region codes Intl.DisplayNames
// knows; resolution then decides.
const capabilityLocalesScript = `(() => {
	const exact = tag => {
		try {
			return Intl.getCanonicalLocales(tag)[0] === tag &&
				new Intl.DateTimeFormat(tag).resolvedOptions().locale === tag &&
				new Intl.NumberFormat(tag).resolvedOptions().locale === tag;
		} catch {
			return false;
		}
	};
	const collates = tag => {
		const resolved = new Intl.Collator(tag).resolvedOptions().locale;
		return resolved === tag || tag.startsWith(resolved + '-');
	};
	const names = type => new Intl.DisplayNames('en', {type, fallback: 'none'});
	const known = (displayNames, code) => {
		try {
			return displayNames.of(code) !== undefined;
		} catch {
			return false;
		}
	};
	const languageNames = names('language');
	const regionNames = names('region');
	const scriptNames = names('script');
	const letters = 'abcdefghijklmnopqrstuvwxyz';
	const codes = [];
	for (const a of letters) for (const b of letters) {
		codes.push(a + b);
		for (const c of letters) codes.push(a + b + c);
	}
	const languages = codes.filter(code => known(languageNames, code)).filter(exact);
	const regions = codes.filter(code => code.length === 2).map(code => code.toUpperCase()).filter(code => known(regionNames, code));
	regions.push('001', '150', '419');
	const scripts = [];
	for (const a of letters.toUpperCase()) for (const b of letters) for (const c of letters) for (const d of letters) {
		if (known(scriptNames, a + b + c + d)) scripts.push(a + b + c + d);
	}
	const prefixes = [...languages];
	for (const language of languages) for (const script of scripts) {
		if (exact(language + '-' + script)) prefixes.push(language + '-' + script);
	}
	const accepted = [];
	for (const prefix of prefixes) {
		if (collates(prefix)) accepted.push(prefix);
		for (const region of regions) {
			const tag = prefix + '-' + region;
			if (exact(tag) && collates(tag)) accepted.push(tag);
		}
	}
	return JSON.stringify(accepted);
})()`

const capabilityTimeZonesScript = `(ids => JSON.stringify(ids.filter(id => {
	try {
		new Intl.DateTimeFormat('en', {timeZone: id});
		return true;
	} catch {
		return false;
	}
})))`

// TestBrowserLocationCapabilities regenerates the capability manifest from the
// running browser and image. Set UPDATE_BROWSER_LOCATION_CAPABILITIES=1 to
// write it; otherwise the test fails when the committed manifest is stale for
// the same Chromium version, and skips images with a different version.
func TestBrowserLocationCapabilities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	page, err := openLocationPage(ctx, c.CDPURL())
	require.NoError(t, err)
	defer page.Close(ctx)

	versionRaw, err := page.client.Call(ctx, "Browser.getVersion", nil, "")
	require.NoError(t, err)
	product, err := decodeJSONStringField(versionRaw, "product")
	require.NoError(t, err)
	_, version, ok := strings.Cut(product, "/")
	require.True(t, ok, "unexpected product %q", product)
	// Locale support depends on the Chromium version's ICU data, not on the
	// product name, which differs between builds of the same version.
	browser := "Chromium/" + version
	update := os.Getenv("UPDATE_BROWSER_LOCATION_CAPABILITIES") == "1"
	if !update && browser != browserlocation.Current().Browser {
		t.Skipf("capabilities describe %s, image runs %s", browserlocation.Current().Browser, browser)
	}

	var locales []string
	require.NoError(t, page.evalJSON(ctx, capabilityLocalesScript, &locales))

	tzdata, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `sed -n 's/^# version //p' /usr/share/zoneinfo/tzdata.zi`})
	require.NoError(t, err)
	zoneFiles, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `cd /usr/share/zoneinfo && find . -path ./posix -prune -o -path ./right -prune -o \( -type f -o -type l \) -print | sed 's|^\./||' | grep -v '\.'`})
	require.NoError(t, err)
	candidates, err := json.Marshal(strings.Fields(zoneFiles))
	require.NoError(t, err)
	var timezones []string
	require.NoError(t, page.evalJSON(ctx, fmt.Sprintf("%s(%s)", capabilityTimeZonesScript, candidates), &timezones))

	generated := browserlocation.New(browser, strings.TrimSpace(tzdata), locales, timezones)
	if update {
		data, err := json.MarshalIndent(generated, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile("../lib/browserlocation/capabilities.json", append(data, '\n'), 0o644))
		t.Logf("wrote %d locales and %d timezones (%s)", len(generated.Locales), len(generated.TimeZones), generated.Version)
		return
	}
	require.Equal(t, generated, browserlocation.Current(), "capability manifest is stale; regenerate it with UPDATE_BROWSER_LOCATION_CAPABILITIES=1")
}

// locationPage is a CDP session attached to a blank page.
type locationPage struct {
	client    *cdpClient
	targetID  string
	sessionID string
}

func openLocationPage(ctx context.Context, wsURL string) (*locationPage, error) {
	client, err := newCDPClient(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	targetRaw, err := client.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("Target.createTarget: %w", err)
	}
	targetID, err := decodeJSONStringField(targetRaw, "targetId")
	if err != nil {
		client.Close()
		return nil, err
	}
	attachRaw, err := client.Call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("Target.attachToTarget: %w", err)
	}
	sessionID, err := decodeJSONStringField(attachRaw, "sessionId")
	if err != nil {
		client.Close()
		return nil, err
	}
	return &locationPage{client: client, targetID: targetID, sessionID: sessionID}, nil
}

// evalJSON evaluates an expression that yields a JSON string, awaiting
// promises, and decodes it into out.
func (p *locationPage) evalJSON(ctx context.Context, expression string, out any) error {
	raw, err := p.client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, p.sessionID)
	if err != nil {
		return fmt.Errorf("Runtime.evaluate: %w", err)
	}
	var envelope struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode Runtime.evaluate result: %w", err)
	}
	if len(envelope.ExceptionDetails) > 0 {
		return fmt.Errorf("evaluation raised an exception: %s", envelope.ExceptionDetails)
	}
	return json.Unmarshal([]byte(envelope.Result.Value), out)
}

func (p *locationPage) Close(ctx context.Context) {
	_, _ = p.client.Call(ctx, "Target.closeTarget", map[string]any{"targetId": p.targetID}, "")
	p.client.Close()
}
