import json
import pathlib
import select
import socket
import struct
import time

from common import config, datagram_capsule, open_tunnel, parse_capsules, udp_targets


def log(**values):
    print(json.dumps(values), flush=True)


def stun_address(data):
    if len(data) < 20 or data[:2] != b'\x01\x01' or data[4:8] != bytes.fromhex('2112a442'):
        return None
    offset = 20
    while offset + 4 <= len(data):
        kind, length = struct.unpack('!HH', data[offset:offset + 4])
        value = data[offset + 4:offset + 4 + length]
        if kind == 32 and length == 8 and len(value) == 8 and value[1] == 1:
            return socket.inet_ntoa(bytes(a ^ b for a, b in zip(value[4:], bytes.fromhex('2112a442'))))
        offset += 4 + ((length + 3) // 4) * 4
    return None


def main():
    settings = config()
    deadline = time.monotonic() + 30
    while not pathlib.Path('/run/udp-egress-ready').exists():
        if time.monotonic() > deadline:
            raise RuntimeError('browser network did not become ready')
        time.sleep(0.1)
    listeners = {}
    for index, target in enumerate(udp_targets(settings)):
        listener = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        listener.bind(('192.0.2.1', 40000 + index))
        listeners[listener] = (target['host'], target['port'])
    flows = {}
    tunnels = {}

    def close(key):
        flow = flows.pop(key)
        tunnels.pop(flow['tunnel'], None)
        flow['tunnel'].close()

    def deliver(key, data):
        flow = flows[key]
        payloads, flow['buffer'] = parse_capsules(flow['buffer'] + data)
        for payload in payloads:
            flow['listener'].sendto(payload, key[0])
            observed = stun_address(payload)
            if observed:
                log(direction='return', receiver=key[1], transaction=payload[8:20].hex(),
                    server_observed_ip=observed)

    log(state='ready', egress=f'{settings["egress_host"]}:{settings["egress_port"]}')
    while True:
        ready, _, _ = select.select(list(listeners) + list(tunnels), [], [], 1)
        for sock in ready:
            if sock in listeners:
                data, source = sock.recvfrom(65535)
                destination = listeners[sock]
                key = (source, destination)
                if key not in flows:
                    if len(flows) >= 32:
                        log(drop='flow_limit')
                        continue
                    try:
                        tunnel, rest = open_tunnel(settings, destination)
                    except OSError as error:
                        log(drop='tunnel_failed', destination=destination, error=str(error))
                        continue
                    flows[key] = {'tunnel': tunnel, 'listener': sock, 'buffer': b'', 'last': time.monotonic()}
                    tunnels[tunnel] = key
                    log(tunnel='open', source=source, destination=destination)
                    if rest:
                        deliver(key, rest)
                flows[key]['last'] = time.monotonic()
                try:
                    flows[key]['tunnel'].sendall(datagram_capsule(data))
                except OSError:
                    close(key)
                    continue
                if len(data) >= 20 and data[:2] == b'\x00\x01' and data[4:8] == bytes.fromhex('2112a442'):
                    log(direction='out', destination=destination, transaction=data[8:20].hex())
            else:
                key = tunnels.get(sock)
                if key is None:
                    continue
                try:
                    data = sock.recv(65536)
                except OSError:
                    data = b''
                if not data:
                    log(tunnel='closed', destination=key[1])
                    close(key)
                    continue
                flows[key]['last'] = time.monotonic()
                deliver(key, data)
        for key, flow in list(flows.items()):
            if time.monotonic() - flow['last'] > 60:
                close(key)


if __name__ == '__main__':
    main()
