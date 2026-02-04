#!/usr/bin/env tsx

import { writeFileSync } from 'fs';
import { Browser, BrowserContext, chromium, Page } from 'playwright-core';

interface CommandOptions {
  wsURL?: string;
  timeout?: number;
}

interface NavigateCookieOptions extends CommandOptions {
  url: string;
  cookieName: string;
  cookieValue: string;
  label?: string;
}

interface NavigateCookieFormOptions extends CommandOptions {
  url: string;
  cookieName: string;
  cookieValue: string;
  label?: string;
}

interface LocalStorageOptions extends CommandOptions {
  url: string;
  key: string;
  value: string;
  label?: string;
}

interface HistoryOptions extends CommandOptions {
  urls: string[];
  label?: string;
}

interface NavigateXAndBackOptions extends CommandOptions {
  label?: string;
}

interface ScreenshotOptions extends CommandOptions {
  filename: string;
}

class CDPClient {
  private browser?: Browser;
  private context?: BrowserContext;
  private page?: Page;

  async connect(wsURL: string = 'ws://127.0.0.1:9222/'): Promise<void> {
    try {
      // Connect to existing browser via CDP
      this.browser = await chromium.connectOverCDP(wsURL);

      // Get the default context (or first available context)
      const contexts = this.browser.contexts();
      if (contexts.length > 0) {
        this.context = contexts[0];
      } else {
        // This shouldn't happen with an existing browser, but just in case
        this.context = await this.browser.newContext();
      }

      // Get existing page or create new one
      const pages = this.context.pages();
      if (pages.length > 0) {
        this.page = pages[0];
      } else {
        this.page = await this.context.newPage();
      }
    } catch (error) {
      console.error('Failed to connect to browser:', error);
      throw error;
    }
  }

  async navigateAndEnsureCookie(options: NavigateCookieOptions): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    const { url, cookieName, cookieValue, label = 'check', timeout = 45000 } = options;

    // Array to collect browser console logs
    const browserLogs: string[] = [];
    // Handler to push logs from browser console
    const consoleListener = (msg: any) => {
      // Only log 'log', 'warn', 'error', 'info' types
      if (['log', 'warn', 'error', 'info'].includes(msg.type())) {
        // Join all arguments as string
        const text = msg.text();
        browserLogs.push(`[browser][${msg.type()}] ${text}`);
      }
    };

    try {
      console.log(`[cdp] action: navigate-cookie, url: ${url}, label: ${label}`);

      // Attach console listener
      this.page.on('console', consoleListener);

      // Set timeout for this operation
      this.page.setDefaultTimeout(timeout);

      // Navigate to the URL
      await this.page.goto(url, { waitUntil: 'domcontentloaded' });

      // Wait for #cookies element to be visible
      await this.page.waitForSelector('#cookies', { state: 'visible', timeout: 5000 });

      // Get the text content of #cookies element
      const cookiesText = await this.page.textContent('#cookies');

      // Echo browser console logs
      if (browserLogs.length > 0) {
        for (const log of browserLogs) {
          console.log(log);
        }
      }

      if (!cookiesText) {
        throw new Error('#cookies element has no text content');
      }

      // Check if the cookie exists with the expected value
      const expectedCookie = `${cookieName}=${cookieValue}`;
      if (!cookiesText.includes(expectedCookie)) {
        // Take a screenshot on failure
        const screenshotPath = `cookie-verify-miss-${label}.png`;
        await this.captureScreenshot({ filename: screenshotPath });
        throw new Error(`Expected document.cookie to contain "${expectedCookie}", got "${cookiesText}"`);
      }

      console.log(`Cookie verified successfully: ${cookieName}=${cookieValue}`);

    } catch (error) {
      // Echo browser console logs on error as well
      if (browserLogs.length > 0) {
        for (const log of browserLogs) {
          console.log(log);
        }
      }
      // Take a screenshot on any error
      const screenshotPath = `cookie-verify-${label}.png`;
      await this.captureScreenshot({ filename: screenshotPath }).catch(console.error);
      throw error;
    } finally {
      // Remove the console listener to avoid leaks
      if (this.page) {
        this.page.off('console', consoleListener);
      }
    }
  }

  async setAndVerifyLocalStorage(options: LocalStorageOptions): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    const { url, key, value, label = 'localstorage', timeout = 45000 } = options;

    try {
      console.log(`[cdp] action: set-localstorage, url: ${url}, key: ${key}, value: ${value}, label: ${label}`);

      // Set timeout for this operation
      this.page.setDefaultTimeout(timeout);

      // Navigate to the URL
      await this.page.goto(url, { waitUntil: 'domcontentloaded' });

      // Set localStorage value
      await this.page.evaluate(({ k, v }: { k: string; v: string }) => {
        (globalThis as any).localStorage.setItem(k, v);
        console.log(`[localStorage] Set ${k}=${v}`);
      }, { k: key, v: value });

      // Verify localStorage value
      const storedValue = await this.page.evaluate(({ k }: { k: string }) => {
        return (globalThis as any).localStorage.getItem(k);
      }, { k: key });

      if (storedValue !== value) {
        const screenshotPath = `localstorage-verify-miss-${label}.png`;
        await this.captureScreenshot({ filename: screenshotPath });
        throw new Error(`Expected localStorage["${key}"] to be "${value}", got "${storedValue}"`);
      }

      console.log(`LocalStorage verified successfully: ${key}=${value}`);

      // Navigate to google.com to potentially force a flush
      console.log('[cdp] action: navigating to google.com to force localStorage flush');
      try {
        await this.page.goto('https://www.google.com', { waitUntil: 'domcontentloaded' });
        console.log('[cdp] action: google.com navigation completed');
      } catch (navError) {
        console.warn('[cdp] action: google.com navigation failed, continuing anyway:', navError);
      }
    } catch (error) {
      const screenshotPath = `localstorage-verify-${label}.png`;
      await this.captureScreenshot({ filename: screenshotPath }).catch(console.error);
      throw error;
    }
  }

  async verifyLocalStorage(options: LocalStorageOptions): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    const { url, key, value, label = 'localstorage-verify', timeout = 45000 } = options;

    try {
      console.log(`[cdp] action: verify-localstorage, url: ${url}, key: ${key}, expected: ${value}, label: ${label}`);

      // Set timeout for this operation
      this.page.setDefaultTimeout(timeout);

      // Navigate to the URL
      await this.page.goto(url, { waitUntil: 'domcontentloaded' });

      // Get localStorage value
      const storedValue = await this.page.evaluate(({ k }: { k: string }) => {
        return (globalThis as any).localStorage.getItem(k);
      }, { k: key });

      if (storedValue !== value) {
        const screenshotPath = `localstorage-verify-fail-${label}.png`;
        await this.captureScreenshot({ filename: screenshotPath });
        throw new Error(`Expected localStorage["${key}"] to be "${value}", got "${storedValue}"`);
      }

      console.log(`LocalStorage verification successful: ${key}=${value}`);
    } catch (error) {
      const screenshotPath = `localstorage-verify-fail-${label}.png`;
      await this.captureScreenshot({ filename: screenshotPath }).catch(console.error);
      throw error;
    }
  }

  async navigateToXAndBack(options: NavigateXAndBackOptions): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    const { label = 'x-navigation', timeout = 45000 } = options;

    try {
      console.log(`[cdp] action: navigate-to-x-and-back, label: ${label}`);

      // Set timeout for this operation
      this.page.setDefaultTimeout(timeout);

      // Do the navigation to x.com and back twice in a loop
      for (let i = 0; i < 2; i++) {
        console.log(`[cdp] action: [${i + 1}/2] navigating to x.com`);
        await this.page.goto('https://x.com', { waitUntil: 'domcontentloaded' });

        // Wait a bit to ensure cookies are set
        await this.page.waitForTimeout(2000);

        console.log(`[cdp] action: [${i + 1}/2] navigating to news.ycombinator.com`);
        await this.page.goto('https://news.ycombinator.com', { waitUntil: 'domcontentloaded' });

        // Wait a bit to ensure the navigation is recorded
        await this.page.waitForTimeout(2000);
      }

      console.log('X.com navigation and return completed successfully');

    } catch (error) {
      const screenshotPath = `x-navigation-${label}.png`;
      await this.captureScreenshot({ filename: screenshotPath }).catch(console.error);
      throw error;
    }
  }

  async captureScreenshot(options: ScreenshotOptions): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    const { filename } = options;

    try {
      // Take a full page screenshot
      const screenshot = await this.page.screenshot({
        fullPage: true,
        type: 'png',
      });

      // Write to file
      writeFileSync(filename, screenshot);
      console.log(`Screenshot saved to: ${filename}`);
    } catch (error) {
      console.error('Failed to capture screenshot:', error);
      throw error;
    }
  }

  async navigateAndVerifyTitleContains(url: string, substring: string, timeout: number = 45000): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    try {
      console.log(`[cdp] action: navigate-and-verify-title, url: ${url}, contains: ${substring}`);
      this.page.setDefaultTimeout(timeout);
      await this.page.goto(url, { waitUntil: 'domcontentloaded' });

      // Give content scripts a small window to run
      await this.page.waitForTimeout(1500);

      const title = await this.page.title();
      console.log(`[cdp] page title: ${title}`);
      if (!title.includes(substring)) {
        const screenshotPath = `title-verify-fail.png`;
        await this.captureScreenshot({ filename: screenshotPath });
        throw new Error(`Expected page title to include "${substring}", got: ${title}`);
      }
      console.log('[cdp] title verification successful');
    } catch (error) {
      const screenshotPath = `title-verify-error.png`;
      await this.captureScreenshot({ filename: screenshotPath }).catch(console.error);
      throw error;
    }
  }

  async verifyMV3ServiceWorker(options: CommandOptions): Promise<void> {
    if (!this.page) throw new Error('Not connected to browser');

    const { timeout = 60000 } = options;

    try {
      console.log('[cdp] action: verify-mv3-service-worker');
      this.page.setDefaultTimeout(timeout);

      // Step 1: Navigate to chrome://extensions
      console.log('[cdp] navigating to chrome://extensions');
      await this.page.goto('chrome://extensions');
      await this.page.waitForTimeout(2000);

      // Step 2: Enable developer mode by clicking the toggle
      console.log('[cdp] enabling developer mode');
      const devMode = this.page.getByRole('button', { name: 'Developer mode' });
      await devMode.click();
      await this.page.waitForTimeout(1000);

      // Step 3: Find the extension and extract the ID
      // chrome://extensions uses shadow DOM, so we need to use evaluate to pierce it
      console.log('[cdp] checking for MV3 Service Worker Test extension');

      const extensionInfo = await this.page.evaluate(() => {
        // Get the extensions-manager element
        const manager = document.querySelector('extensions-manager');
        if (!manager || !manager.shadowRoot) return null;

        // Get the item list
        const itemList = manager.shadowRoot.querySelector('extensions-item-list');
        if (!itemList || !itemList.shadowRoot) return null;

        // Find all extension items
        const items = itemList.shadowRoot.querySelectorAll('extensions-item');

        for (const item of items) {
          if (!item.shadowRoot) continue;

          // Get the extension name
          const nameEl = item.shadowRoot.querySelector('#name');
          const name = nameEl?.textContent?.trim() || '';

          if (name === 'MV3 Service Worker Test') {
            // Get the extension ID from the item's id attribute
            const id = item.getAttribute('id');

            // Check if service worker is inactive
            const inspectViews = item.shadowRoot.querySelector('#inspect-views');
            const isInactive = inspectViews?.textContent?.includes('(Inactive)') || false;

            // Check if service worker link exists
            const hasServiceWorker = inspectViews?.textContent?.includes('service worker') || false;

            return { id, name, isInactive, hasServiceWorker };
          }
        }

        return null;
      });

      if (!extensionInfo) {
        await this.captureScreenshot({ filename: 'mv3-extension-not-found.png' });
        throw new Error('MV3 Service Worker Test extension not found on chrome://extensions');
      }

      console.log(`[cdp] found extension: ${extensionInfo.name} (ID: ${extensionInfo.id})`);
      console.log(`[cdp] has service worker: ${extensionInfo.hasServiceWorker}, inactive: ${extensionInfo.isInactive}`);

      if (!extensionInfo.hasServiceWorker) {
        await this.captureScreenshot({ filename: 'mv3-no-service-worker.png' });
        throw new Error('Extension does not have a service worker registered');
      }

      if (extensionInfo.isInactive) {
        await this.captureScreenshot({ filename: 'mv3-service-worker-inactive.png' });
        throw new Error('Service worker is marked as (Inactive)');
      }

      console.log('[cdp] service worker is active');

      // Step 4: Navigate to the extension's popup
      const extensionId = extensionInfo.id;
      const popupUrl = `chrome-extension://${extensionId}/popup.html`;
      console.log(`[cdp] navigating to popup: ${popupUrl}`);
      await this.page.goto(popupUrl);
      await this.page.waitForTimeout(1000);

      // Step 5: Click the "Ping Service Worker" button
      console.log('[cdp] clicking Ping Service Worker button');
      const pingButton = this.page.getByRole('button', { name: 'Ping Service Worker' });
      await pingButton.click();
      await this.page.waitForTimeout(2000);

      // Step 6: Verify the status shows success
      const statusElement = this.page.locator('#status');
      const statusText = await statusElement.textContent();
      console.log(`[cdp] status text: ${statusText}`);

      if (!statusText || !statusText.includes('SUCCESS')) {
        await this.captureScreenshot({ filename: 'mv3-ping-failed.png' });
        throw new Error(`Expected status to show SUCCESS, got: ${statusText}`);
      }

      if (!statusText.includes('Service worker is alive')) {
        await this.captureScreenshot({ filename: 'mv3-wrong-message.png' });
        throw new Error(`Expected status to include "Service worker is alive", got: ${statusText}`);
      }

      console.log('[cdp] MV3 service worker verification successful!');
      await this.captureScreenshot({ filename: 'mv3-success.png' });

    } catch (error) {
      console.error('[cdp] MV3 service worker verification failed:', error);
      await this.captureScreenshot({ filename: 'mv3-verification-error.png' }).catch(console.error);
      throw error;
    }
  }

  async disconnect(): Promise<void> {
    // Note: We don't close the browser since it's an existing instance
    // We just disconnect from it
    if (this.browser) {
      await this.browser.close().catch(() => {
        // Ignore errors when disconnecting
      });
    }
  }
}

async function main(): Promise<void> {
  const args = process.argv.slice(2);

  if (args.length === 0) {
    console.error('Usage: tsx index.ts <command> [options]');
    console.error('Commands:');
    console.error('  navigate-and-ensure-cookie --url <url> --cookie-name <name> --cookie-value <value> [--label <label>] [--ws-url <ws>] [--timeout <ms>]');
    console.error('  set-localstorage --url <url> --key <key> --value <value> [--label <label>] [--ws-url <ws>] [--timeout <ms>]');
    console.error('  verify-localstorage --url <url> --key <key> --value <value> [--label <label>] [--ws-url <ws>] [--timeout <ms>]');
    console.error('  navigate-to-x-and-back [--label <label>] [--ws-url <ws>] [--timeout <ms>]');
    process.exit(1);
  }

  const command = args[0];
  const options: Record<string, string> = {};

  // Parse command line arguments
  for (let i = 1; i < args.length; i += 2) {
    const key = args[i];
    const value = args[i + 1];
    if (key.startsWith('--') && value) {
      options[key.substring(2)] = value;
    }
  }

  const client = new CDPClient();

  try {
    // Connect to browser
    const wsURL = options['ws-url'] || 'ws://127.0.0.1:9222/';
    await client.connect(wsURL);

    switch (command) {
      case 'navigate-and-ensure-cookie': {
        if (!options.url || !options['cookie-name'] || !options['cookie-value']) {
          throw new Error('Missing required options: --url, --cookie-name, --cookie-value');
        }

        await client.navigateAndEnsureCookie({
          wsURL,
          url: options.url,
          cookieName: options['cookie-name'],
          cookieValue: options['cookie-value'],
          label: options.label,
          timeout: options.timeout ? parseInt(options.timeout, 10) : undefined,
        });
        break;
      }

      case 'set-localstorage': {
        if (!options.url || !options.key || !options.value) {
          throw new Error('Missing required options: --url, --key, --value');
        }

        await client.setAndVerifyLocalStorage({
          wsURL,
          url: options.url,
          key: options.key,
          value: options.value,
          label: options.label,
          timeout: options.timeout ? parseInt(options.timeout, 10) : undefined,
        });
        break;
      }

      case 'verify-localstorage': {
        if (!options.url || !options.key || !options.value) {
          throw new Error('Missing required options: --url, --key, --value');
        }

        await client.verifyLocalStorage({
          wsURL,
          url: options.url,
          key: options.key,
          value: options.value,
          label: options.label,
          timeout: options.timeout ? parseInt(options.timeout, 10) : undefined,
        });
        break;
      }

      case 'navigate-to-x-and-back': {
        await client.navigateToXAndBack({
          wsURL,
          label: options.label,
          timeout: options.timeout ? parseInt(options.timeout, 10) : undefined,
        });
        break;
      }

      case 'verify-title-contains': {
        if (!options.url || !options.substr) {
          throw new Error('Missing required options: --url, --substr');
        }
        await client.navigateAndVerifyTitleContains(
          options.url,
          options.substr,
          options.timeout ? parseInt(options.timeout, 10) : undefined,
        );
        break;
      }

      case 'verify-mv3-service-worker': {
        await client.verifyMV3ServiceWorker({
          wsURL,
          timeout: options.timeout ? parseInt(options.timeout, 10) : undefined,
        });
        break;
      }

      default:
        throw new Error(`Unknown command: ${command}`);
    }

    process.exit(0);
  } catch (error) {
    console.error('Error:', error);
    process.exit(1);
  } finally {
    await client.disconnect();
  }
}

// Run the main function
main().catch((error) => {
  console.error('Unhandled error:', error);
  process.exit(1);
});
