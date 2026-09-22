([
  {
    kind: 'cdp',
    match: {url_patterns: ['https://www.google.com/travel/flights*']},
    tool: {
      name: 'search_flights',
      description: 'Search live Google Flights results for one adult, one-way economy travel between two IATA airport codes on a specific date. Read-only; does not book a flight.',
      annotations: {readOnlyHint: true},
      inputSchema: {
        type: 'object',
        properties: {
          origin: {type: 'string', description: 'Three-letter origin airport IATA code, e.g. SFO'},
          destination: {type: 'string', description: 'Three-letter destination airport IATA code, e.g. LAX'},
          departure_date: {type: 'string', description: 'Departure date in YYYY-MM-DD format'},
        },
        required: ['origin', 'destination', 'departure_date'],
        additionalProperties: false,
      },
      outputSchema: {
        type: 'object',
        properties: {
          search_url: {type: 'string'},
          flights: {
            type: 'array',
            items: {type: 'object', properties: {description: {type: 'string'}}, required: ['description'], additionalProperties: false},
          },
        },
        required: ['search_url', 'flights'],
        additionalProperties: false,
      },
    },
    execute: async (input, {signal}) => {
      const {origin, destination, departure_date: date} = input;
      if (!/^[A-Z]{3}$/.test(origin) || !/^[A-Z]{3}$/.test(destination) || origin === destination) {
        throw new Error('origin and destination must be different three-letter IATA codes');
      }
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date) ||
          new Date(`${date}T00:00:00Z`).toISOString().slice(0, 10) !== date ||
          date < new Date().toISOString().slice(0, 10)) {
        throw new Error('departure_date must be a valid future YYYY-MM-DD date');
      }
      const field = (number, bytes) => [number << 3 | 2, bytes.length, ...bytes];
      const text = (number, value) => field(number, [...new TextEncoder().encode(value)]);
      const leg = [
        ...text(2, date),
        ...field(13, text(2, origin)),
        ...field(14, text(2, destination)),
      ];
      const bytes = [...field(3, leg), 64, 1, 72, 1, 152, 1, 2];
      const tfs = Buffer.from(bytes).toString('base64url');
      const url = `https://www.google.com/travel/flights/search?tfs=${tfs}&hl=en&gl=US&curr=USD`;
      const original = await browser.currentTab();
      let resultTab;
      try {
        resultTab = await browser.newTab(url);
        const found = await browser.waitForElement('ul[role="list"] li [role="link"][aria-label*="Select flight"]', {timeoutSec: 35});
        if (signal.aborted) throw new Error('flight search canceled');
        if (!found) throw new Error('Google Flights did not display flight results');
        const result = await browser.js(`(() => ({
          title: document.title,
          flights: [...document.querySelectorAll('ul[role="list"] li [role="link"][aria-label*="Select flight"]')]
            .slice(0, 5)
            .map(node => ({description: node.getAttribute('aria-label')})),
        }))()`);
        if (!result?.title?.includes('Google Flights') || !result.flights?.length) {
          throw new Error('Google Flights returned an unrecognized result page');
        }
        return {search_url: url, flights: result.flights};
      } finally {
        if (resultTab && resultTab !== original.targetId) await browser.closeTab(resultTab);
        await browser.switchTab(original.targetId);
      }
    },
  },
])
