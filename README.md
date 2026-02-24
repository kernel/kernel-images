<p align="center">
  <img src="static/images/logo-kernel-light.svg" alt="Kernel Logo" width="55%">
</p>

<p align="center">
  <img alt="GitHub License" src="https://img.shields.io/github/license/onkernel/kernel-images">
  <a href="https://discord.gg/FBrveQRcud"><img src="https://img.shields.io/discord/1342243238748225556?logo=discord&logoColor=white&color=7289DA" alt="Discord"></a>
  <a href="https://x.com/juecd__"><img src="https://img.shields.io/twitter/follow/juecd__" alt="Follow @juecd__"></a>
  <a href="https://x.com/rfgarcia"><img src="https://img.shields.io/twitter/follow/rfgarcia" alt="Follow @rfgarcia"></a>
</p>

## What's Kernel?

Kernel provides sandboxed, ready-to-use Chrome browsers for browser automations and web agents. This repo powers our [hosted services](https://kernel.sh/docs/introduction).

Sign up [here](https://www.kernel.sh/)!

## Key Features

- Sandboxed Chrome browser that Chrome DevTools-based browser frameworks (Playwright, Puppeteer) can connect to
- Remote GUI access (live view streaming) for visual monitoring and remote control
- Configurable live view settings (read-only view, browser window dimensions)
- Controllable video replays of the browser's session

## What You Can Do With It

- Run automated browser-based workflows
- Develop and test AI agents that use browsers
- Build custom tools that require controlled browser environments

## Implementation

This image can be used to run headful Chromium in a Docker container or with Unikraft unikernels. The unikernel implementation builds on top of the base Docker image and has the additional benefits of running on a unikernel:

- Automated standby / "sleep mode" when there is no network activity (consuming negligible resources when it does)
- When it goes into standby mode, the unikernel’s state gets snapshotted and can be restored exactly as it was when it went to sleep. This could be useful if you want to reuse a session’s state (browser auth cookies, interact with local files, browser settings, even the exact page and window zoom you were on).
- Extremely fast cold restarts (<20ms), which could be useful for any application that requires super low latency event handlers.

## Demo

https://github.com/user-attachments/assets/5888e823-5867-4c01-ad67-ec8989ba9573

## Running in Docker

You can build and run the Dockerfile directly as a Docker container.

```sh
cd images/chromium-headful
IMAGE=kernel-docker ./build-docker.sh
IMAGE=kernel-docker ENABLE_WEBRTC=true ./run-docker.sh
```

## Running on a Unikernel

Alternatively, you can run the browser on a Unikraft unikernel.

### 1. Install the Kraft CLI
`curl -sSfL https://get.kraftkit.sh | sh`

### 2. Add Unikraft Secret to Your CLI
`export UKC_METRO=<region>`
`export UKC_TOKEN=<secret>`

### 3. Build the image
`IMAGE=YOUR_UKC_USERNAME/chromium-headless-test:latest images/chromium-headless/build-unikernel.sh`

### 4. Run it
`IMAGE=YOUR_UKC_USERNAME/chromium-headless-test:latest images/chromium-headless/run-unikernel.sh`
or
`IMAGE=YOUR_UKC_USERNAME/chromium-headful-test:latest VOLIMPORT_PREFIX=official images/chromium-headful/run-unikernel.sh`

When the deployment finishes successfully, the Kraft CLI will print out something like this:
```
Deployed successfully!
 │
 ├───────── name: kernel-cu
 ├───────── uuid: 0cddb958...
 ├──────── metro: <region>
 ├──────── state: starting
 ├─────── domain: https://<service_name>.kraft.host
 ├──────── image: onkernel/kernel-cu@sha256:8265f3f188...
 ├─────── memory: 8192 MiB
 ├────── service: <service_name>
 ├─ private fqdn: <id>
 ├─── private ip: <ip>
 └───────── args: /wrapper.sh
```

### Unikernel Notes

- The image requires at least 8gb of memory.
- To deploy the implementation with WebRTC desktop streaming enabled instead of noVNC: `ENABLE_WEBRTC=true NEKO_ICESERVERS=xxx ./run-unikernel.sh`
- Deploying to Unikraft Cloud requires the usage of a [TURN server](https://webrtc.org/getting-started/turn-server) when `ENABLE_WEBRTC=true`, as direct exposure of UDP ports is not currently supported. `NEKO_ICESERVERS`: Describes multiple STUN and TURN server that can be used by the ICEAgent to establish a connection with a peer. e.g. `[{"urls": ["turn:turn.example.com:19302", "stun:stun.example.com:19302"], "username": "name", "credential": "password"}, {"urls": ["stun:stun.example2.com:19302"]}]`.
- Various services (mutter, tint) take a few seconds to start-up. Once they do, the standby and restart time is extremely fast.
- The Unikraft deployment generates a url. This url is public, meaning _anyone_ can access the remote GUI if they have the url. Only use this for non-sensitive browser interactions, and delete the unikernel instance when you're done.
- You can call `browser.close()` to disconnect to the browser, and the unikernel will go into standby after network activity ends. You can then reconnect to the instance using CDP. `browser.close()` ends the websocket connection but doesn't actually close the browser.
- VCPUS value can be adjusted using the variable: `VCPUS=8`

## Connect to the browser via Chrome DevTools Protocol

Port `9222` is exposed via `ncat`, allowing you to connect Chrome DevTools Protocol-based browser frameworks like Playwright and Puppeteer (and CDP-based SDKs like Browser Use). You can use these frameworks to drive the browser in the cloud. You can also disconnect from the browser and reconnect to it.

First, fetch the browser's CDP websocket endpoint:

```typescript
const url = new URL("http://localhost:9222/json/version");
const response = await fetch(url, {
  headers: {
    "Host": "<this can be anything>" // Required if using a unikernel
  }
});
if (response.status !== 200) {
  throw new Error(
    `Failed to retrieve browser instance: ${
      response.statusText
    } ${await response.text()}`
  );
}
// webSocketDebuggerUrl should look like:
// ws:///devtools/browser/06acd5ef-9961-431d-b6a0-86b99734f816
const { webSocketDebuggerUrl } = await response.json();
```

Then, connect a remote Playwright or Puppeteer client to it:

```typescript
// Puppeteer
const browser = await puppeteer.connect({
  browserWSEndpoint: webSocketDebuggerUrl,
});
// Playwright
const browser = await chromium.connectOverCDP(webSocketDebuggerUrl);
```

## Browser Remote GUI / Live View

You can use the embedded live view to monitor and control the browser. The live view supports both read and write access to the browser. Both map to port `443`.

- NoVNC: A VNC client. Read/write is supported. Set `ENABLE_WEBRTC=false` in `./run-docker.sh`.
- WebRTC: A WebRTC-based client. Read/write, window resizing, and copy/paste is supported. It's much faster than VNC. Available when `ENABLE_WEBRTC=true` is set.

### Notes
- Audio streaming in the WebRTC implementation is currently non-functional and needs to be fixed.
- The live view is read/write by default. You can set it to read-only by adding `-e ENABLE_READONLY_VIEW=true \` in `docker run`.

## Replay Capture

You can use the embedded recording server to capture recordings of the entire screen in our headful images. It allows for one recording at a time and can be enabled with `WITH_KERNEL_IMAGES_API=true`

For example:

```bash
cd images/chromium-headful
export IMAGE=kernel-docker
./build-docker.sh
WITH_KERNEL_IMAGES_API=true ENABLE_WEBRTC=true ./run-docker.sh

# 1. Start a new recording
curl http://localhost:10001/recording/start -d {}

# recording in progress - run your agent

# 2. Stop recording
curl http://localhost:10001/recording/stop -d {}

# 3. Download the recorded file
curl http://localhost:10001/recording/download --output recording.mp4
```

Note: the recording file is encoded into a H.264/MPEG-4 AVC video file. [QuickTime has known issues with playback](https://discussions.apple.com/thread/254851789?sortBy=rank) so please make sure to use a compatible media player!

## Documentation

This repo powers our managed [browser infrastructure](https://kernel.sh/docs).

## Contributing

Please read our [contribution guidelines](./CONTRIBUTING.md) before submitting pull requests or issues.

## License

See the [LICENSE](./LICENSE) file for details.

## Support

For issues, questions, or feedback, please [open an issue](https://github.com/onkernel/kernel-images/issues) on this repository. You can also join our [Discord](https://discord.gg/FBrveQRcud).

## Colophon

- Our WebRTC implementation is adapted from [Neko](https://github.com/m1k1o/neko).
- Thank you to [xonkernel](https://github.com/xonkernel) for leading the development of our WebRTC live view.
- Thank you to the [Unikraft Cloud](https://unikraft.cloud/) team for your help with unikernels.

Made with ❤️ by the [Kernel team](https://www.kernel.sh).
