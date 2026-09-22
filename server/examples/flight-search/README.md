# Flight-search WebMCP proof

One read-only custom WebMCP registration on Google Flights. Both the Jev and LLM demo planners discover the same live `tool_ref` and invoke it via `/webmcp/invoke`; neither contains a separate flight-search executor.

Requires a browser image with the custom WebMCP API, Bun, and credentials for the planner being tested. Open `https://www.google.com/travel/flights?hl=en&gl=US&curr=USD` in that browser before installing.

```sh
bun server/examples/flight-search/demo.mjs install
JEV_API_KEY=... bun server/examples/flight-search/demo.mjs jev 'Find flights SFO to LAX on 2026-10-15'
OPENAI_API_KEY=... bun server/examples/flight-search/demo.mjs llm 'Find flights SFO to LAX on 2026-10-15'
bun test server/examples/flight-search/*.test.*
```

Set `BROWSER_API_URL` if the browser API is not at `http://127.0.0.1:10001`. Installing submits the source to the local browser instance; do not expose that API to untrusted callers. Navigating away removes the registration from the page and returning reinstalls it. The registration is not yet a hosted/signed catalog entry and is not restored after the Browser REPL process restarts.

The prototype accepts explicit future ISO dates and a small airport candidate set. New York and Chicago without an airport are clarified, not guessed. Jev uses TypeSafe Choice questions for routing and airport selection; low-confidence decisions ask for clarification. The LLM uses the live WebMCP tool schema. Both planners validate their selected codes and date against deterministic candidates before invocation. The tool opens a temporary browser tab, navigates to Google Flights, reads up to five observed flight descriptions, then closes the tab. `cdp` execution is required because the tool navigates to a new page while its WebMCP registration must stay alive on the original page.

Google Flights' search URL and result markup are not public contracts. Failure to find results returns an error, not fabricated flight data. This proof does not cover booking, round trips, a comprehensive airport resolver, hosted delivery, signing, restart recovery, or production site reliability.
