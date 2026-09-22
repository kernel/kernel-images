import {readFile} from 'node:fs/promises';

const api = process.env.BROWSER_API_URL ?? 'http://127.0.0.1:10001';
const namespace = 'kernel.flight-search-demo';
const airports = {
  SFO: ['SFO', 'San Francisco'], LAX: ['LAX', 'Los Angeles'],
  SEA: ['SEA', 'Seattle'], BOS: ['BOS', 'Boston'],
  JFK: ['JFK', 'JFK Airport'], EWR: ['EWR', 'Newark'], LGA: ['LGA', 'LaGuardia'],
  ORD: ['ORD', "O'Hare"], MDW: ['MDW', 'Midway'],
};
const cities = { 'New York': ['JFK', 'EWR', 'LGA'], Chicago: ['ORD', 'MDW'] };

export function candidatesFor(request) {
  const codes = new Set();
  for (const [code, aliases] of Object.entries(airports)) {
    if (aliases.some((alias) => new RegExp(`\\b${alias}\\b`, 'i').test(request))) codes.add(code);
  }
  const ambiguousCities = [];
  for (const [city, options] of Object.entries(cities)) {
    if (!new RegExp(`\\b${city}\\b`, 'i').test(request)) continue;
    const explicit = options.filter((code) => codes.has(code));
    if (explicit.length === 0) {
      ambiguousCities.push(city);
      options.forEach((code) => codes.add(code));
    }
  }
  const dates = [...new Set(request.match(/\b\d{4}-\d{2}-\d{2}\b/g) ?? [])]
    .filter((date) => !Number.isNaN(Date.parse(`${date}T00:00:00Z`)) &&
      new Date(`${date}T00:00:00Z`).toISOString().slice(0, 10) === date &&
      date >= new Date().toISOString().slice(0, 10));
  return {airports: [...codes], dates, ambiguousCities};
}

async function jsonRequest(url, init) {
  const response = await fetch(url, init);
  const body = await response.json();
  if (!response.ok) throw new Error(`${response.status} ${JSON.stringify(body)}`);
  return body;
}

async function liveTool() {
  const {tools} = await jsonRequest(`${api}/webmcp/tools`);
  const match = tools.find(({source, tool}) => source.custom?.namespace === namespace && tool.name === 'search_flights');
  if (!match) throw new Error('flight-search WebMCP tool is not registered on the current page');
  return match;
}

async function jevPlan(request, candidates) {
  const options = Object.fromEntries(candidates.airports.map((code) => [code, airports[code].join(' / ')]));
  const result = await jsonRequest('https://api.typesafe.ai/v1/systemone', {
    method: 'POST',
    headers: {'content-type': 'application/json', authorization: `Bearer ${process.env.JEV_API_KEY}`},
    body: JSON.stringify({
      model: 'jev-latest',
      state: {request, airports: options, dates: candidates.dates},
      questions: {
        intent: {type: 'choice', instructions: 'Is the user asking to search for flights without booking?', criteria: {search_flights: 'Find flight options', other: 'Anything else'}},
        origin: {type: 'choice', instructions: 'Which airport is the departure origin in the request? Choose none if not clear.', criteria: {...options, none: 'Origin is not specified or cannot be resolved to one airport'}},
        destination: {type: 'choice', instructions: 'Which airport is the arrival destination in the request? Choose none if not clear.', criteria: {...options, none: 'Destination is not specified or cannot be resolved to one airport'}},
        ...(candidates.dates.length > 1 ? {date: {type: 'choice', instructions: 'Which date is the departure date, not the return date? Choose none if unclear.', criteria: {...Object.fromEntries(candidates.dates.map((d) => [d, null])), none: 'Departure date unclear'}}} : {}),
      },
    }),
  });
  const {intent, origin, destination, date} = result.answers;
  if (intent.choice !== 'search_flights' || intent.confidence < 0.75) return {clarification: 'Do you want me to search for flights?', usage: result.usage};
  if ([origin, destination, date].filter(Boolean).some(({choice, confidence}) => choice === 'none' || confidence < 0.75) || origin.choice === destination.choice) {
    return {clarification: 'Which exact origin and destination airports, and departure date, should I use?', usage: result.usage};
  }
  return {input: {origin: origin.choice, destination: destination.choice, departure_date: date?.choice ?? candidates.dates[0]}, usage: result.usage};
}

async function llmPlan(request, candidates, {tool}) {
  const result = await jsonRequest('https://api.openai.com/v1/responses', {
    method: 'POST',
    headers: {'content-type': 'application/json', authorization: `Bearer ${process.env.OPENAI_API_KEY}`},
    body: JSON.stringify({
      model: 'gpt-4.1-mini',
      input: `Today's UTC date: ${new Date().toISOString().slice(0, 10)}. Request: ${request}\nAvailable airport codes: ${candidates.airports.join(', ')}. Explicit future dates: ${candidates.dates.join(', ')}. If the request lacks an unambiguous airport or date, do not call the tool. Never invent an airport or date.`,
      tools: [{type: 'function', name: 'search_flights', description: tool.description, parameters: tool.inputSchema}],
      tool_choice: 'auto',
      parallel_tool_calls: false,
    }),
  });
  const call = result.output.find((item) => item.type === 'function_call' && item.name === 'search_flights');
  return call ? {input: JSON.parse(call.arguments), usage: result.usage} : {clarification: 'Please specify exact origin and destination airports and a departure date in YYYY-MM-DD.', usage: result.usage};
}

export function validatePlan(input, candidates) {
  return input && candidates.airports.includes(input.origin) &&
    candidates.airports.includes(input.destination) && input.origin !== input.destination &&
    candidates.dates.includes(input.departure_date);
}

async function main() {
  const [mode, ...parts] = process.argv.slice(2);
  if (mode === 'install') {
    const source = await readFile(new URL('./tool.js', import.meta.url), 'utf8');
    console.log(JSON.stringify(await jsonRequest(`${api}/webmcp/custom-tools`, {
      method: 'POST', headers: {'content-type': 'application/json'},
      body: JSON.stringify({namespace, source}),
    }), null, 2));
    return;
  }
  if (!['jev', 'llm'].includes(mode) || !parts.length) throw new Error('usage: node demo.mjs install | jev|llm "flight request with YYYY-MM-DD date"');
  const request = parts.join(' ');
  const candidates = candidatesFor(request);
  if (candidates.ambiguousCities.length) {
    console.log(JSON.stringify({clarification: `Which airport in ${candidates.ambiguousCities.join(' and ')} do you mean?`}));
    return;
  }
  if (candidates.airports.length < 2 || candidates.dates.length === 0) {
    console.log(JSON.stringify({clarification: 'Specify exact origin and destination airports and a future date in YYYY-MM-DD.'}));
    return;
  }
  const live = await liveTool();
  const plan = mode === 'jev' ? await jevPlan(request, candidates) : await llmPlan(request, candidates, live);
  if (plan.clarification || !validatePlan(plan.input, candidates)) {
    console.log(JSON.stringify({clarification: plan.clarification ?? 'The planner could not resolve the requested airports or date.', usage: plan.usage}));
    return;
  }
  const result = await jsonRequest(`${api}/webmcp/invoke`, {
    method: 'POST', headers: {'content-type': 'application/json'},
    body: JSON.stringify({tool_ref: live.tool_ref, input: plan.input, timeout_sec: 55}),
  });
  console.log(JSON.stringify({planner: mode, tool_ref: live.tool_ref, input: plan.input, usage: plan.usage, result}, null, 2));
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  main().catch((error) => { console.error(error.message); process.exitCode = 1; });
}
