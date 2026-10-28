ARG BASE_IMAGE=kernel-images:isp-base
FROM ${BASE_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 iptables python3 socat tcpdump && rm -rf /var/lib/apt/lists/*
COPY experiments/udp-egress/*.py experiments/udp-egress/*.cjs experiments/udp-egress/chromium-launcher.sh /opt/udp-egress/
RUN chmod +x /opt/udp-egress/chromium-launcher.sh && \
    sed -i 's|command=/usr/local/bin/chromium-launcher|command=/opt/udp-egress/chromium-launcher.sh|' /etc/supervisor/conf.d/services/chromium.conf && \
    cd /opt/udp-egress && PYTHONDONTWRITEBYTECODE=1 python3 -m unittest -v test_protocol.py
COPY experiments/udp-egress/network.conf /etc/supervisor/conf.d/services/udp-egress.conf
