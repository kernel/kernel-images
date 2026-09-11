#!/usr/bin/env bash
set -euxo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get --no-install-recommends -y install \
    fuse-overlayfs \
    iptables

# Docker bridge networking requires the legacy xtables backend in browser VMs.
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

apt-get clean
rm -rf /var/lib/apt/lists/*
