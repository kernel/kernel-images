FROM docker.io/golang:1.25.0 AS server-builder
WORKDIR /workspace/server

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-golang1250
ENV CGO_ENABLED=0

# Go module dependencies first for better layer caching
COPY server/go.mod ./
COPY server/go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build,id=$CACHEIDPREFIX-go-build \
    --mount=type=cache,target=/go/pkg/mod,id=$CACHEIDPREFIX-go-pkg-mod \
    go mod download

COPY server/ .

# Build kernel-images API
RUN --mount=type=cache,target=/root/.cache/go-build,id=$CACHEIDPREFIX-go-build \
    --mount=type=cache,target=/go/pkg/mod,id=$CACHEIDPREFIX-go-pkg-mod \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o /out/kernel-images-api ./cmd/api

# Build chromium launcher
RUN --mount=type=cache,target=/root/.cache/go-build,id=$CACHEIDPREFIX-go-build \
    --mount=type=cache,target=/go/pkg/mod,id=$CACHEIDPREFIX-go-pkg-mod \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o /out/chromium-launcher ./cmd/chromium-launcher

# Build container entrypoint wrapper (replaces wrapper.sh)
RUN --mount=type=cache,target=/root/.cache/go-build,id=$CACHEIDPREFIX-go-build \
    --mount=type=cache,target=/go/pkg/mod,id=$CACHEIDPREFIX-go-pkg-mod \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o /out/wrapper ./cmd/wrapper

# Build supervisord eventlistener shim
RUN --mount=type=cache,target=/root/.cache/go-build,id=$CACHEIDPREFIX-go-build \
    --mount=type=cache,target=/go/pkg/mod,id=$CACHEIDPREFIX-go-pkg-mod \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o /out/kernel-images-supervisord-shim ./cmd/supervisord-shim

FROM docker.io/ubuntu:22.04 AS ffmpeg-downloader

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-ubuntu2204

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    set -xe; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache; \
    apt-get -yqq update; \
    apt-get -yqq --no-install-recommends install ca-certificates curl xz-utils;

# Download a pinned FFmpeg static build for the recording server
RUN --mount=type=cache,target=/tmp/cache/ffmpeg,sharing=locked,id=$CACHEIDPREFIX-ffmpeg \
    <<-'EOT'
    set -eux
    FFMPEG_CACHE_PATH="/tmp/cache/ffmpeg"
    FFMPEG_RELEASE="autobuild-2026-07-31-14-10"
    FFMPEG_VERSION="n7.1.5-12-g1fdbca85aa"
    case ${TARGETARCH:-amd64} in
        "amd64") FFMPEG_TARGET_ARCH="64" ;;
        "arm64") FFMPEG_TARGET_ARCH="arm64" ;;
    esac
    FFMPEG_TARGET=linux${FFMPEG_TARGET_ARCH:?}
    ARCHIVE_NAME="ffmpeg-${FFMPEG_VERSION}-${FFMPEG_TARGET}-gpl-7.1.tar.xz"
    FFMPEG_CACHED_ARCHIVE_PATH="$FFMPEG_CACHE_PATH/$ARCHIVE_NAME"
    FFMPEG_CACHED_ARCHIVE_CHECKSUM_PATH="$FFMPEG_CACHED_ARCHIVE_PATH.sha256"
    RELEASE_URL="https://github.com/BtbN/FFmpeg-Builds/releases/download/$FFMPEG_RELEASE"
    URL="$RELEASE_URL/$ARCHIVE_NAME"
    TEMPORARY_SHA256_CHECKSUM_PATH=$(mktemp /tmp/tmp_sha256.XXXXXXXXXX)
    CONTINUE="true"
    echo "Downloading FFmpeg checksum"
    if curl --connect-timeout 10 -fsSL "$RELEASE_URL/checksums.sha256" -o $TEMPORARY_SHA256_CHECKSUM_PATH; then
        grep -F "$ARCHIVE_NAME" $TEMPORARY_SHA256_CHECKSUM_PATH > $FFMPEG_CACHED_ARCHIVE_CHECKSUM_PATH
    else
        echo "Failed to connect to ffmpeg static build provider for checksum."
        echo "Checking for cached version to use."
        if [ -f "$FFMPEG_CACHED_ARCHIVE_CHECKSUM_PATH" ]; then
            echo "Found cached checksum."
        else
            echo "Unable to locate cached checksum."
            CONTINUE="false"
        fi
    fi
    rm $TEMPORARY_SHA256_CHECKSUM_PATH

    if [ "$CONTINUE" = "false" ]; then
        exit 1
    fi

    echo "Checking cache for FFmpeg archive and validating checksum"
    if (cd $FFMPEG_CACHE_PATH && sha256sum --check $FFMPEG_CACHED_ARCHIVE_CHECKSUM_PATH); then
        echo "Checksum validated, using cached FFmpeg archive"
    else
        echo "Downloading FFmpeg static build from $URL"
        curl -fsSL "$URL" -o $FFMPEG_CACHED_ARCHIVE_PATH
        echo "Validating checksum of FFmpeg static build download"
        (cd $FFMPEG_CACHE_PATH && sha256sum --check $FFMPEG_CACHED_ARCHIVE_CHECKSUM_PATH)
    fi

    tar -xJf $FFMPEG_CACHED_ARCHIVE_PATH -C /tmp
    install -m755 /tmp/ffmpeg-*/bin/ffmpeg  /usr/local/bin/ffmpeg
    install -m755 /tmp/ffmpeg-*/bin/ffprobe /usr/local/bin/ffprobe
    rm -rf /tmp/ffmpeg*
EOT

FROM node:22-bullseye-slim AS node-22
FROM docker.io/ubuntu:22.04

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-ubuntu2204

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    set -xe; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache; \
    apt-get -yqq update; \
    apt-get -yqq --no-install-recommends install \
    libcups2 \
    libnss3 \
    libatk1.0-0 \
    libnspr4 \
    libpango1.0-0 \
    libasound2 \
    libatspi2.0-0 \
    libxdamage1 \
    libatk-bridge2.0-0 \
    libxkbcommon0 \
    libdrm2 \
    libxcomposite1 \
    libxfixes3 \
    libxrandr2 \
    libgbm1 \
    libdbus-1-3 \
    libglib2.0-0 \
    libgtk-3-0 \
    libxext6; \
    apt-get -yqq --no-install-recommends install \
    ca-certificates \
    curl \
    gpg \
    dbus \
    dbus-x11 \
    pulseaudio \
    xvfb \
    x11-utils \
    x11-xserver-utils \
    xclip \
    xdotool \
    fontconfig \
    locales \
    tzdata \
    fonts-noto-cjk \
    fonts-noto-color-emoji \
    fonts-noto-mono \
    fonts-nanum \
    supervisor; \
    locale-gen en_US.UTF-8 en_GB.UTF-8 en_CA.UTF-8 en_IE.UTF-8 en_SG.UTF-8; \
    fc-cache -f

# sqlite3 for debugging the cookies file; unzip for the chrome-for-testing archives below.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    apt-get update -y && \
    apt-get --no-install-recommends -y install sqlite3 unzip;

# Install Chrome + ChromeDriver as a matched pair from chrome-for-testing so they cannot drift apart.
# Regional pack filenames preserve each locale identifier while reusing en-GB UI strings.
ARG CHROME_VERSION=152.0.7977.42
RUN set -eux; \
    curl -fsSL "https://storage.googleapis.com/chrome-for-testing-public/${CHROME_VERSION}/linux64/chrome-linux64.zip" -o /tmp/chrome.zip; \
    curl -fsSL "https://storage.googleapis.com/chrome-for-testing-public/${CHROME_VERSION}/linux64/chromedriver-linux64.zip" -o /tmp/cd.zip; \
    unzip -q /tmp/chrome.zip -d /opt; \
    unzip -q /tmp/cd.zip -d /tmp; \
    mv /opt/chrome-linux64 /opt/chrome-for-testing; \
    mv /opt/chrome-for-testing/chrome /opt/chrome-for-testing/chromium; \
    for locale in en-CA en-IE en-SG; do cp /opt/chrome-for-testing/locales/en-GB.pak "/opt/chrome-for-testing/locales/${locale}.pak"; done; \
    ln -sf /opt/chrome-for-testing/chromium /usr/bin/chromium; \
    mv /tmp/chromedriver-linux64/chromedriver /usr/local/bin/chromedriver; \
    chmod +x /usr/local/bin/chromedriver /opt/chrome-for-testing/chromium; \
    rm -rf /tmp/chrome.zip /tmp/cd.zip /tmp/chromedriver-linux64

# Chrome for Testing reads managed policies from /etc/opt/chrome_for_testing/policies, but
# the server writes them to /etc/chromium/policies. Symlink so writes flow to the real path.
RUN mkdir -p /etc/chromium/policies/managed /etc/opt/chrome_for_testing && \
    ln -sfn /etc/chromium/policies /etc/opt/chrome_for_testing/policies
COPY shared/chromium-policies/managed/policy.json /etc/chromium/policies/managed/policy.json

# Install FFmpeg (latest static build) for the recording server
COPY --from=ffmpeg-downloader /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-downloader /usr/local/bin/ffprobe /usr/local/bin/ffprobe

# Remove upower to prevent spurious D-Bus activations and logs
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    apt-get -yqq purge upower || true

# install Node.js 22.x by copying from the node:22-bullseye-slim stage
COPY --from=node-22 /usr/local/bin/node /usr/local/bin/node
COPY --from=node-22 /usr/local/lib/node_modules /usr/local/lib/node_modules
# Recreate symlinks for npm/npx/corepack to point into node_modules
RUN set -eux; \
    ln -sf /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm; \
    ln -sf /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx; \
    if [ -e /usr/local/lib/node_modules/corepack/dist/corepack.js ]; then \
    ln -sf /usr/local/lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack; \
    fi

# Install TypeScript, Playwright, Patchright, esbuild globally
RUN --mount=type=cache,target=/root/.npm,id=$CACHEIDPREFIX-npm npm install -g typescript playwright-core patchright esbuild

# Pre-install openssh-server and websocat for faster SSH setup via kernel CLI.
# Only installed, NOT configured: no host keys, no custom config, service not enabled.
RUN apt-get update && \
    apt-get --no-install-recommends -y install openssh-server && \
    rm -rf /var/lib/apt/lists/* && \
    rm -f /etc/ssh/ssh_host_* && \
    ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
        amd64) WEBSOCAT_ARCH="x86_64" ;; \
        arm64) WEBSOCAT_ARCH="aarch64" ;; \
        *) echo "Unsupported architecture: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/vi/websocat/releases/download/v1.14.1/websocat.${WEBSOCAT_ARCH}-unknown-linux-musl" \
        -o /usr/local/bin/websocat && \
    chmod +x /usr/local/bin/websocat

ENV WITHDOCKER=true

# Create a non-root user with a home directory
RUN useradd -m -s /bin/bash kernel

# supervisor  start scripts
COPY images/chromium-headless/image/start-xvfb.sh /images/chromium-headless/image/start-xvfb.sh
RUN chmod +x /images/chromium-headless/image/start-xvfb.sh
COPY shared/start-pulseaudio.sh /usr/local/bin/start-pulseaudio.sh
RUN chmod +x /usr/local/bin/start-pulseaudio.sh

# Container entrypoint wrapper (Go binary, replaces wrapper.sh)
COPY --from=server-builder /out/wrapper /wrapper

# Supervisord configuration
COPY images/chromium-headless/image/supervisord.conf /etc/supervisor/supervisord.conf
COPY images/chromium-headless/image/supervisor/services/ /etc/supervisor/conf.d/services/
COPY shared/envoy/supervisor-envoy.conf /etc/supervisor/conf.d/services/envoy.conf

# Install Envoy proxy
COPY shared/envoy/install-proxy.sh /usr/local/bin/install-proxy.sh
RUN chmod +x /usr/local/bin/install-proxy.sh && /usr/local/bin/install-proxy.sh && rm /usr/local/bin/install-proxy.sh

# Copy Envoy configuration files
COPY shared/envoy/bootstrap.yaml /etc/envoy/templates/bootstrap.yaml
COPY shared/envoy/init-envoy.sh /usr/local/bin/init-envoy.sh
RUN chmod +x /usr/local/bin/init-envoy.sh

# Bake the envoy forward-proxy CA cert into the image (system trust store +
# NSS DB for both root and kernel users). See bake-certs.sh for the security
# rationale on sharing the cert across containers built from this image.
COPY shared/envoy/bake-certs.sh /usr/local/bin/bake-certs.sh
RUN chmod +x /usr/local/bin/bake-certs.sh && /usr/local/bin/bake-certs.sh && rm /usr/local/bin/bake-certs.sh

# Copy the kernel-images API binary built in the builder stage
COPY --from=server-builder /out/kernel-images-api /usr/local/bin/kernel-images-api
COPY --from=server-builder /out/chromium-launcher /usr/local/bin/chromium-launcher
COPY --from=server-builder /out/kernel-images-supervisord-shim /usr/local/bin/kernel-images-supervisord-shim

# Copy and compile the Playwright daemon
COPY server/runtime/playwright-daemon.ts server/runtime/page-target-id-cache.ts server/runtime/webmcp.ts /tmp/
RUN esbuild /tmp/playwright-daemon.ts \
    --bundle \
    --platform=node \
    --target=node22 \
    --format=esm \
    --outfile=/usr/local/lib/playwright-daemon.js \
    --external:playwright-core \
    --external:patchright \
    --external:esbuild \
    && rm /tmp/playwright-daemon.ts /tmp/page-target-id-cache.ts /tmp/webmcp.ts

# Copy and install the browser REPL's pinned runtime dependencies before bundling.
COPY server/runtime/ /tmp/browser-repl/
RUN npm ci --ignore-scripts --no-audit --no-fund --omit=dev --prefix /tmp/browser-repl \
    && mkdir -p /usr/local/lib/browser-repl /usr/local/share/doc/kernel-images/meriyah /usr/local/share/doc/kernel-images/playwright-core /usr/local/share/doc/kernel-images/sharp \
    && cp /tmp/browser-repl/node_modules/meriyah/LICENSE.md /usr/local/share/doc/kernel-images/meriyah/LICENSE.md \
    && cp /tmp/browser-repl/node_modules/meriyah/package.json /usr/local/share/doc/kernel-images/meriyah/package.json \
    && cp /tmp/browser-repl/node_modules/playwright-core/LICENSE /usr/local/share/doc/kernel-images/playwright-core/LICENSE \
    && cp /tmp/browser-repl/node_modules/playwright-core/NOTICE /usr/local/share/doc/kernel-images/playwright-core/NOTICE \
    && cp /tmp/browser-repl/node_modules/playwright-core/package.json /usr/local/share/doc/kernel-images/playwright-core/package.json \
    && cp /tmp/browser-repl/node_modules/sharp/LICENSE /usr/local/share/doc/kernel-images/sharp/LICENSE \
    && cp /tmp/browser-repl/node_modules/sharp/package.json /usr/local/share/doc/kernel-images/sharp/package.json \
    && cp -a /tmp/browser-repl/node_modules /usr/local/lib/browser-repl/node_modules \
    && esbuild /tmp/browser-repl/browser-repl.ts \
      --bundle \
      --platform=node \
      --target=node22 \
      --format=cjs \
      --supported:dynamic-import=true \
      --external:sharp \
      --outfile=/usr/local/lib/browser-repl/browser-repl.js \
    && rm -rf /tmp/browser-repl

ENTRYPOINT [ "/wrapper" ]
