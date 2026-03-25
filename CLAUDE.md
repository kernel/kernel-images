# kernel-images

## Deployment

- Merging to main does **not** automatically deploy to production.
- CI builds Docker images (`onkernel/chromium-headful:<short-sha>` and `onkernel/chromium-headless:<short-sha>`), runs e2e tests, and pushes to Docker Hub. That's where CI ends.
- Production runs unikernel images on KraftCloud, not Docker images directly.
- To deploy to production:
  1. Build the unikernel image using `build-unikernel.sh`.
  2. Push to KraftCloud.
  3. Update the image SHA reference in `kernel/kernel`. The Go module dependency `github.com/onkernel/kernel-images/server` is used in `packages/api`, `packages/metro-api`, and `packages/integration`.
  4. Redeploy the API.
- The image references in `kernel/kernel` are Go module dependencies — update them with `go get` and `go mod tidy`.
