import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('./tool.js', import.meta.url), 'utf8');

function load(browser) {
  const [tool] = vm.runInNewContext(source, {browser, Buffer, Date, TextEncoder});
  return tool;
}

test('encodes a one-way flight search and returns observed page results', async () => {
  const calls = [];
  const tool = load({
    currentTab: async () => ({targetId: 'original'}),
    newTab: async (url) => { calls.push(['newTab', url]); return 'results'; },
    waitForElement: async () => true,
    js: async () => ({title: 'San Francisco to Los Angeles | Google Flights', flights: [{description: 'Observed flight'}]}),
    closeTab: async (id) => calls.push(['closeTab', id]),
    switchTab: async (id) => calls.push(['switchTab', id]),
  });
  assert.equal(tool.kind, 'cdp');
  assert.deepEqual(Array.from(tool.match.url_patterns), ['https://www.google.com/travel/flights*']);
  const result = await tool.execute({origin: 'SFO', destination: 'LAX', departure_date: '2026-10-15'}, {signal: new AbortController().signal});
  assert.equal(result.flights[0].description, 'Observed flight');
  assert.match(result.search_url, /tfs=GhoSCjIwMjYtMTAtMTVqBRIDU0ZPcgUSA0xBWEABSAGYAQI/);
  assert.deepEqual(calls.map(([method]) => method), ['newTab', 'closeTab', 'switchTab']);
});

test('rejects malformed arguments before opening a tab', async () => {
  const tool = load({});
  for (const input of [
    {origin: 'SFO', destination: 'SFO', departure_date: '2026-10-15'},
    {origin: 'SFO', destination: 'LAX', departure_date: '2026-02-30'},
    {origin: 'NYC!', destination: 'LAX', departure_date: '2026-10-15'},
  ]) {
    await assert.rejects(tool.execute(input, {signal: new AbortController().signal}));
  }
});

test('fails closed when results are missing and restores the original tab', async () => {
  const calls = [];
  const tool = load({
    currentTab: async () => ({targetId: 'original'}),
    newTab: async () => 'results',
    waitForElement: async () => false,
    closeTab: async (id) => calls.push(['closeTab', id]),
    switchTab: async (id) => calls.push(['switchTab', id]),
  });
  await assert.rejects(
    tool.execute({origin: 'SFO', destination: 'LAX', departure_date: '2026-10-15'}, {signal: new AbortController().signal}),
    /did not display flight results/,
  );
  assert.deepEqual(calls, [['closeTab', 'results'], ['switchTab', 'original']]);
});
