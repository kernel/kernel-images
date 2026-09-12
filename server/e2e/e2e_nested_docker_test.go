package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNestedDockerExecUsesContainerRoot(t *testing.T) {
	if backendKindFromEnv() != BackendHypeman {
		t.Skip("nested Docker rootfs regression requires the Hypeman backend")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start Hypeman instance")
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		require.NoError(t, c.Stop(cleanupCtx), "failed to stop Hypeman instance")
	}()
	require.NoError(t, c.WaitReady(ctx), "browser API not ready")

	const script = `set -euxo pipefail
rm -f /var/run/docker.pid /var/run/docker.sock
rm -rf /var/lib/docker-exec-e2e /run/docker-exec-e2e

dockerd \
  --host=unix:///var/run/docker.sock \
  --data-root=/var/lib/docker-exec-e2e \
  --exec-root=/run/docker-exec-e2e \
  >/tmp/docker-exec-e2e.log 2>&1 &
dockerd_pid=$!
cleanup() {
  docker rm -f nested-exec-e2e >/dev/null 2>&1 || true
  kill "$dockerd_pid" >/dev/null 2>&1 || true
  wait "$dockerd_pid" >/dev/null 2>&1 || true
}
trap cleanup EXIT

for _ in $(seq 1 60); do
  if docker info >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! docker info >/dev/null 2>&1; then
  cat /tmp/docker-exec-e2e.log >&2
  exit 1
fi

docker pull alpine:3.20 >/dev/null
docker run -d --name nested-exec-e2e alpine:3.20 \
  sh -c 'echo pid1-created >/runtime-created; exec sleep 300' >/dev/null
container_pid=$(docker inspect --format '{{.State.Pid}}' nested-exec-e2e)
for _ in $(seq 1 50); do
  if test -f "/proc/$container_pid/root/runtime-created"; then
    break
  fi
  sleep 0.1
done

pid1_image=$(cat "/proc/$container_pid/root/etc/alpine-release")
exec_image=$(docker exec nested-exec-e2e cat /etc/alpine-release)
printf 'pid1 image: %s\ndocker exec image: %s\n' "$pid1_image" "$exec_image"
test "$exec_image" = "$pid1_image"
test "$(docker exec nested-exec-e2e cat /runtime-created)" = pid1-created

docker exec nested-exec-e2e sh -c 'echo exec-created >/exec-created'
test "$(docker exec nested-exec-e2e cat /exec-created)" = exec-created
`

	exitCode, output, err := c.Exec(ctx, []string{"bash", "-lc", script})
	require.NoError(t, err, "failed to execute nested Docker regression")
	require.Equalf(t, 0, exitCode, "nested docker exec used the wrong rootfs:\n%s", output)
}
