const { Builder, Browser } = require('selenium-webdriver');
const chrome = require('selenium-webdriver/chrome');
const LogInspector = require('selenium-webdriver/bidi/logInspector');

const endpoint = getArg('--endpoint');
if (!endpoint) {
    console.error('Usage: node test-selenium-bidi.js --endpoint <http://host:port>');
    process.exit(1);
}

async function main() {
    const options = new chrome.Options();
    options.enableBidi();

    const driver = await new Builder()
        .forBrowser(Browser.CHROME)
        .setChromeOptions(options)
        .usingServer(endpoint)
        .setCapability('unhandledPromptBehavior', { default: 'ignore' })
        .build();

    console.log('Session created');

    // 1. Get browsing context
    const handle = await driver.getWindowHandle();
    console.log('Window handle:', handle);
    if (!handle) {
        throw new Error('expected a non-empty window handle');
    }

    // 2. Test console log event subscription
    const logInspector = await LogInspector(driver);
    const consolePromise = new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('Timed out waiting for console event')), 10000);
        logInspector.onConsoleEntry((entry) => {
            clearTimeout(timer);
            resolve(entry);
        });
    });
    await driver.executeScript("console.log('hello from selenium bidi')");
    const consoleEntry = await consolePromise;
    console.log('console event:', consoleEntry.text);
    if (consoleEntry.text !== 'hello from selenium bidi') {
        throw new Error(`expected console text "hello from selenium bidi", got "${consoleEntry.text}"`);
    }

    // 3. Navigate and verify title
    await driver.get('https://example.com');
    const title = await driver.getTitle();
    console.log('Title:', title);
    if (!title.includes('Example Domain')) {
        throw new Error(`expected title to contain "Example Domain", got "${title}"`);
    }

    // 4. Evaluate script
    const result = await driver.executeScript('return document.title');
    console.log('executeScript document.title:', result);
    if (!result.includes('Example Domain')) {
        throw new Error(`expected executeScript result to contain "Example Domain", got "${result}"`);
    }

    await logInspector.close();
    await driver.quit();
    console.log('All tests passed!');
    process.exit(0);
}

main().catch((err) => {
    console.error('Test failed:', err);
    process.exit(1);
});

function getArg(name) {
    const idx = process.argv.indexOf(name);
    if (idx !== -1 && idx + 1 < process.argv.length) {
        return process.argv[idx + 1];
    }
    return null;
}
