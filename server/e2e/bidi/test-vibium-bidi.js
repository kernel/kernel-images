/**
 * Vibium BiDi e2e test: connects directly to a remote BiDi WebSocket endpoint
 * and verifies the current remote-browser ergonomics.
 *
 * Usage: node test-vibium-bidi.js --endpoint ws://host:port/session
 */
const { browser } = require('vibium/sync');

const endpoint = getArg('--endpoint');
if (!endpoint) {
  console.error('Usage: node test-vibium-bidi.js --endpoint <ws://host:port/session>');
  process.exit(1);
}

function getArg(name) {
  const idx = process.argv.indexOf(name);
  if (idx !== -1 && idx + 1 < process.argv.length) {
    return process.argv[idx + 1];
  }
  return null;
}

function main() {
  const bro = browser.start(endpoint);
  const page = bro.page();

  page.go('https://example.com');
  const title = page.title();
  if (!title.includes('Example Domain')) {
    throw new Error(`expected title to contain "Example Domain", got "${title}"`);
  }
  console.log('Title:', title);

  const h1Text = page.find('h1').text();
  if (!h1Text.includes('Example Domain')) {
    throw new Error(`expected h1 to contain "Example Domain", got "${h1Text}"`);
  }
  console.log('h1 text:', h1Text);

  bro.stop();
  console.log('All tests passed!');
}

try {
  main();
} catch (err) {
  console.error('Test failed:', err);
  process.exit(1);
}
