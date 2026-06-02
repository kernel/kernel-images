#!/usr/bin/env bash

# This script MUST be sourced (i.e. use `source start-buildkit.sh`).

# Variable used by KraftKit, hence the requirement for sourcing the script
export KRAFTKIT_BUILDKIT_HOST=docker-container://buildkit

cleanup_buildkit() {
    echo "Stopping 'buildkit' container ... "
    docker stop buildkit
}

# Install container if not already installed.
if [ -n "$(docker ps --all --no-trunc --quiet --filter 'name=^buildkit$')" ]; then
    echo "Container 'buildkit' is already installed. Nothing to do."
else
    echo "Installing 'buildkit' container ... "
    docker run -d --rm --name buildkit --privileged moby/buildkit:latest
fi

if [ "$(docker container inspect -f '{{.State.Running}}' buildkit 2> /dev/null)" = "true" ]; then
    echo "Container 'buildkit' is already running. Nothing to do."
else
    echo "Starting 'buildkit' container ... "
    docker start buildkit
fi

trap cleanup_buildkit EXIT
