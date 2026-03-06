const puppeteer = require('puppeteer-core');

const endpoint = getArg('--endpoint');
if (!endpoint) {
    console.error('Usage: node test-puppeteer-bidi.js --endpoint <ws://host:port/session>');
    process.exit(1);
}

async function main() {
    const browser = await puppeteer.connect({
        browserWSEndpoint: endpoint,
        protocol: 'webDriverBiDi',
        capabilities: {
            alwaysMatch: {
                unhandledPromptBehavior: { default: 'ignore' },
            },
        },
    });

    const pages = await browser.pages();
    const page = pages[0];
    console.log('Connected, pages:', pages.length);

    // 1. Test console event
    const consolePromise = new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('Timed out waiting for console event')), 10000);
        page.on('console', (msg) => {
            clearTimeout(timer);
            resolve(msg.text());
        });
    });
    await page.evaluate(() => console.log('hello from puppeteer bidi'));
    const consoleText = await consolePromise;
    console.log('console event:', consoleText);
    if (consoleText !== 'hello from puppeteer bidi') {
        throw new Error(`expected console text "hello from puppeteer bidi", got "${consoleText}"`);
    }

    // 2. Test navigation + title
    await page.goto('https://example.com', { waitUntil: 'load' });
    console.log('navigation complete');

    const title = await page.title();
    console.log('Title:', title);
    if (!title.includes('Example Domain')) {
        throw new Error(`expected title to contain "Example Domain", got "${title}"`);
    }

    await browser.disconnect();
    console.log('All tests passed!');
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
