const { chromium } = require("/usr/local/lib/node_modules/playwright-core");

async function main() {
  const [httpsURL, stunURL, label = "probe"] = process.argv.slice(2);
  if (!httpsURL || !stunURL)
    throw new Error("usage: node probe.cjs HTTPS_ECHO_URL STUN_URL [LABEL]");
  const browser = await chromium.connectOverCDP("http://127.0.0.1:9222");
  try {
    const context = browser.contexts()[0];
    const page = context.pages()[0] || (await context.newPage());
    await page.goto(httpsURL, {
      waitUntil: "domcontentloaded",
      timeout: 30000,
    });
    const echo = JSON.parse(await page.locator("body").innerText());
    const rtc = await page.evaluate(async (url) => {
      const connection = new RTCPeerConnection({ iceServers: [{ urls: url }] });
      const candidates = [];
      const errors = [];
      connection.onicecandidate = ({ candidate }) => {
        if (candidate)
          candidates.push({
            type: candidate.type,
            address: candidate.address,
            port: candidate.port,
            protocol: candidate.protocol,
          });
      };
      connection.onicecandidateerror = (event) =>
        errors.push({ code: event.errorCode, text: event.errorText });
      connection.createDataChannel("udp-egress-test");
      await connection.setLocalDescription(await connection.createOffer());
      await new Promise((resolve) => {
        const timeout = setTimeout(resolve, 12000);
        connection.onicegatheringstatechange = () => {
          if (connection.iceGatheringState === "complete") {
            clearTimeout(timeout);
            resolve();
          }
        };
      });
      const state = connection.iceGatheringState;
      connection.close();
      return { candidates, errors, state };
    }, stunURL);
    const serverReflexive = rtc.candidates.filter(
      (candidate) => candidate.type === "srflx",
    );
    console.log(
      JSON.stringify({
        label,
        browserVersion: browser.version(),
        httpsIP: echo.ip,
        browserIPsMatchHTTPS:
          serverReflexive.length > 0 &&
          serverReflexive.every((candidate) => candidate.address === echo.ip),
        rtc,
      }),
    );
  } finally {
    await browser.close();
  }
}

main().catch((error) => {
  console.error(error.message);
  process.exitCode = 1;
});
