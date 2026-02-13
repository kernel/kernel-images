FROM docker.io/golang:1.25.0 AS server-builder
WORKDIR /workspace/server

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-golang1250
ENV CGO_ENABLED=0

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

# webrtc client
FROM node:22-bullseye-slim AS client
WORKDIR /src

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-node22bullseye

COPY images/chromium-headful/client/package*.json ./
RUN --mount=type=cache,target=/root/.npm,id=$CACHEIDPREFIX-npm npm install
COPY images/chromium-headful/client/ .
RUN --mount=type=cache,target=/root/.npm,id=$CACHEIDPREFIX-npm npm run build

# xorg dependencies
FROM docker.io/ubuntu:22.04 AS xorg-deps
WORKDIR /xorg

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-ubuntu2204

ENV DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    set -eux; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache; \
    apt-get update; \
    apt-get --no-install-recommends -y install \
    git gcc pkgconf autoconf automake libtool make xorg-dev xutils-dev;
COPY images/chromium-headful/xorg-deps/ /xorg/
# build xf86-video-dummy v0.3.8 with RandR support
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    set -eux; \
    cd xf86-video-dummy/v0.3.8; \
    patch -p1 < ../01_v0.3.8_xdummy-randr.patch; \
    autoreconf -v --install; \
    ./configure; \
    make -j$(nproc); \
    make install;
# build custom input driver
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    set -eux; \
    cd xf86-input-neko; \
    ./autogen.sh --prefix=/usr; \
    ./configure; \
    make -j$(nproc); \
    make install;

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

FROM ghcr.io/kernel/neko/base:3.0.8-v1.4.0 AS neko
# ^--- now has event.SYSTEM_PONG with legacy support to keepalive
FROM node:22-bullseye-slim AS node-22
FROM docker.io/ubuntu:22.04

# Allow cross-compilation when building with BuildKit platforms
ARG TARGETARCH
ARG TARGETOS
ARG CACHEIDPREFIX=${TARGETOS:-linux}-${TARGETARCH:-amd64}-ubuntu2204

ENV DEBIAN_FRONTEND=noninteractive
ENV DEBIAN_PRIORITY=high

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache; \
    apt-get update && \
    apt-get -y upgrade && \
    apt-get --no-install-recommends -y install  \
    gpg-agent \
    # UI Requirements
    xvfb \
    xterm \
    xdotool \
    scrot \
    imagemagick \
    sudo \
    mutter \
    # Python/pyenv reqs
    build-essential \
    libssl-dev  \
    zlib1g-dev \
    libbz2-dev \
    libreadline-dev \
    libsqlite3-dev \
    curl \
    git \
    libncursesw5-dev \
    xz-utils \
    tk-dev \
    libxml2-dev \
    libxmlsec1-dev \
    libffi-dev \
    liblzma-dev \
    # Network tools
    net-tools \
    netcat \
    # PPA req
    software-properties-common && \
    # Userland apps
    sudo add-apt-repository ppa:mozillateam/ppa && \
    sudo apt-get --no-install-recommends -y install \
    x11-apps \
    tint2 \
    wget \
    xdg-utils \
    libvulkan1 \
    fonts-liberation \
    fonts-noto-cjk \
    fonts-noto-color-emoji \
    fonts-nanum \
    fontconfig \
    unzip && \
    fc-cache -f

# install ffmpeg manually since the version available in apt is from the 4.x branch due to #drama.
COPY --from=ffmpeg-downloader /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-downloader /usr/local/bin/ffprobe /usr/local/bin/ffprobe

# runtime
ENV USERNAME=root
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    set -eux; \
    apt-get update; \
    apt-get --no-install-recommends -y install \
    wget ca-certificates python2 supervisor xclip xdotool unclutter \
    pulseaudio dbus-x11 xserver-xorg-video-dummy \
    libcairo2 libxcb1 libxrandr2 libxv1 libopus0 libvpx7 \
    x11-xserver-utils \
    gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad gstreamer1.0-plugins-ugly \
    gstreamer1.0-pulseaudio gstreamer1.0-omx; \
    #
    # install libxcvt0 (not available in debian:bullseye)
    ARCH=$(dpkg --print-architecture); \
    wget http://ftp.de.debian.org/debian/pool/main/libx/libxcvt/libxcvt0_0.1.2-1_${ARCH}.deb; \
    apt-get --no-install-recommends install ./libxcvt0_0.1.2-1_${ARCH}.deb; \
    rm ./libxcvt0_0.1.2-1_${ARCH}.deb; \
    #
    # workaround for an X11 problem: http://blog.tigerteufel.de/?p=476
    mkdir /tmp/.X11-unix; \
    chmod 1777 /tmp/.X11-unix; \
    chown $USERNAME /tmp/.X11-unix/; \
    #
    # make directories for neko
    mkdir -p /etc/neko /var/www /var/log/neko \
    /tmp/runtime-$USERNAME \
    /home/$USERNAME/.config/pulse  \
    /home/$USERNAME/.local/share/xorg; \
    chmod 1777 /var/log/neko; \
    chown $USERNAME /var/log/neko/ /tmp/runtime-$USERNAME; \
    chown -R $USERNAME:$USERNAME /home/$USERNAME;

# install chromium and sqlite3 for debugging the cookies file
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    add-apt-repository -y ppa:xtradeb/apps;
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked,id=$CACHEIDPREFIX-apt-cache \
    --mount=type=cache,target=/var/lib/apt,sharing=locked,id=$CACHEIDPREFIX-apt-lib \
    apt update -y && \
    apt -y install chromium && \
    apt --no-install-recommends -y install sqlite3;

# Copy Chromium policy configuration
RUN mkdir -p /etc/chromium/policies/managed
COPY shared/chromium-policies/managed/policy.json /etc/chromium/policies/managed/policy.json

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

# setup desktop env & app
ENV DISPLAY_NUM=1
ENV HEIGHT=1080
ENV WIDTH=1920
ENV WITHDOCKER=true

COPY images/chromium-headful/xorg.conf /etc/neko/xorg.conf
COPY images/chromium-headful/neko.yaml /etc/neko/neko.yaml
COPY --from=neko /usr/bin/neko /usr/bin/neko
COPY --from=client /src/dist/ /var/www
COPY --from=xorg-deps /usr/local/lib/xorg/modules/drivers/dummy_drv.so /usr/lib/xorg/modules/drivers/dummy_drv.so
COPY --from=xorg-deps /usr/local/lib/xorg/modules/input/neko_drv.so /usr/lib/xorg/modules/input/neko_drv.so

COPY images/chromium-headful/image-chromium/ /
COPY images/chromium-headful/start-pulseaudio.sh /images/chromium-headful/start-pulseaudio.sh
RUN chmod +x /images/chromium-headful/start-pulseaudio.sh
COPY images/chromium-headful/wrapper.sh /wrapper.sh
COPY images/chromium-headful/supervisord.conf /etc/supervisor/supervisord.conf
COPY images/chromium-headful/supervisor/services/ /etc/supervisor/conf.d/services/
COPY shared/envoy/supervisor-envoy.conf /etc/supervisor/conf.d/services/envoy.conf

# Install Envoy proxy
COPY shared/envoy/install-proxy.sh /usr/local/bin/install-proxy.sh
RUN chmod +x /usr/local/bin/install-proxy.sh && /usr/local/bin/install-proxy.sh && rm /usr/local/bin/install-proxy.sh

# Copy Envoy configuration files
COPY shared/envoy/bootstrap.yaml /etc/envoy/templates/bootstrap.yaml
COPY shared/envoy/init-envoy.sh /usr/local/bin/init-envoy.sh
RUN chmod +x /usr/local/bin/init-envoy.sh

# copy the kernel-images API binary built in the builder stage
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

RUN useradd -m -s /bin/bash kernel

ENTRYPOINT [ "/wrapper.sh" ]
