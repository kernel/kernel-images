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

# Download FFmpeg (latest static build) for the recording server
RUN --mount=type=cache,target=/tmp/cache/ffmpeg,sharing=locked,id=$CACHEIDPREFIX-ffmpeg \
    <<-'EOT'
    set -eux
    FFMPEG_CACHE_PATH="/tmp/cache/ffmpeg"
    case ${TARGETARCH:-amd64} in
        "amd64") FFMPEG_TARGET_ARCH="64" ;;
        "arm64") FFMPEG_TARGET_ARCH="arm64" ;;
    esac
    FFMPEG_TARGET=linux${FFMPEG_TARGET_ARCH:?}
    ARCHIVE_NAME="ffmpeg-n7.1-latest-${FFMPEG_TARGET}-gpl-7.1.tar.xz"
    FFMPEG_CACHED_ARCHIVE_PATH="$FFMPEG_CACHE_PATH/$ARCHIVE_NAME"
    FFMPEG_CACHED_ARCHIVE_CHECKSUM_PATH="$FFMPEG_CACHED_ARCHIVE_PATH.sha256"
    URL="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/$ARCHIVE_NAME"
    TEMPORARY_SHA256_CHECKSUM_PATH=$(mktemp /tmp/tmp_sha256.XXXXXXXXXX)
    CONTINUE="true"
    echo "Downloading FFmpeg checksum"
    if curl --connect-timeout 10 -fsSL "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/checksums.sha256" -o $TEMPORARY_SHA256_CHECKSUM_PATH; then
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
    libnss3; \
    apt-get -yqq --no-install-recommends install \
    ca-certificates \
    curl \
    build-essential \
    libssl-dev \
    git \
    gpg-agent \
    dbus \
    dbus-x11 \
    xvfb \
    x11-utils \
    xdotool \
    fontconfig \
    fonts-noto-cjk \
    fonts-noto-color-emoji \
    fonts-nanum \
    software-properties-common \
    supervisor; \
    fc-cache -f

# install chromium and sqlite3 for debugging the cookies file
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    add-apt-repository -y ppa:xtradeb/apps
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    apt-get update -y && \
    apt-get -y install chromium && \
    apt-get --no-install-recommends -y install sqlite3;

# Copy Chromium policy configuration
RUN mkdir -p /etc/chromium/policies/managed
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

ENV WITHDOCKER=true

# Create a non-root user with a home directory
RUN useradd -m -s /bin/bash kernel

# supervisor  start scripts
COPY images/chromium-headless/image/start-xvfb.sh /images/chromium-headless/image/start-xvfb.sh
RUN chmod +x /images/chromium-headless/image/start-xvfb.sh

# Wrapper script to set environment
COPY images/chromium-headless/image/wrapper.sh /usr/bin/wrapper.sh

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

# Copy the kernel-images API binary built in the builder stage
COPY --from=server-builder /out/kernel-images-api /usr/local/bin/kernel-images-api
COPY --from=server-builder /out/chromium-launcher /usr/local/bin/chromium-launcher

# Copy and compile the Playwright daemon
COPY server/runtime/playwright-daemon.ts /tmp/playwright-daemon.ts
RUN esbuild /tmp/playwright-daemon.ts \
    --bundle \
    --platform=node \
    --target=node22 \
    --format=esm \
    --outfile=/usr/local/lib/playwright-daemon.js \
    --external:playwright-core \
    --external:patchright \
    --external:esbuild \
    && rm /tmp/playwright-daemon.ts

ENTRYPOINT [ "/usr/bin/wrapper.sh" ]
