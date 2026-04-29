## How to test kernel-images changes locally with docker

- Make relevant changes to kernel-images example adding a new endpoint at `kernel-images/server/cmd/api/api/computer.go`, example I added `SetCursor()` endpoint.
- Run openApi to generate the boilerplate for the new endpoints with make oapi-generate
- Check changes at `kernel-images/server/lib/oapi/oapi.go`
- `cd kernel-images/images/chromium-headful`
-  Build and run the docker image with `./build-docker.sh && ./run-docker.sh`
- Open `http://localhost:6080/vnc.html?autoconnect=true&resize=scale` in your browser
- To test the legacy WebRTC/Neko path instead, run `LIVE_VIEW_BACKEND=neko ./run-docker.sh`
- On Apple Silicon / other local `arm64` builds, ChromeDriver is skipped automatically because Google only publishes Linux `chromedriver` for `linux64`. The live view, CDP, and `/computer/*` APIs are still expected to work.
- Now new endpoint should be available for tests example curl command:
```sh
curl -X POST localhost:444/computer/cursor \
  -H "Content-Type: application/json" \
  -d '{"hidden": true}'
```
