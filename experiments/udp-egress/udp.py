import json
import pathlib
import select
import socket
import struct
import time

from common import associate, config, udp_targets


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
        destination = (target['host'], target['port'])
        listener = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        listener.bind(('192.0.2.1', 40000 + index))
        listeners[listener] = destination
    flows = {}
    sockets = {}

    def close(key):
        flow = flows.pop(key)
        for sock in [flow['udp'], flow['control']]:
            sockets.pop(sock, None)
            sock.close()

    log(state='ready')
    while True:
        ready, _, _ = select.select(list(listeners) + list(sockets), [], [], 1)
        for sock in ready:
            if sock in listeners:
                data, source = sock.recvfrom(65535)
                destination = listeners[sock]
                key = (source, destination)
                if key not in flows:
                    if len(flows) >= 32:
                        log(drop='flow_limit')
                        continue
                    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
                    udp.bind(('0.0.0.0', 0))
                    control = None
                    try:
                        control, relay = associate(settings, udp.getsockname()[1])
                        udp.connect(relay)
                    except OSError as error:
                        udp.close()
                        if control is not None:
                            control.close()
                        log(drop='association_failed', error=str(error))
                        continue
                    flows[key] = {'udp': udp, 'control': control, 'listener': sock, 'last': time.monotonic()}
                    sockets[udp] = (key, 'udp')
                    sockets[control] = (key, 'control')
                    log(association=relay, source=source, destination=destination)
                flow = flows[key]
                flow['last'] = time.monotonic()
                packet = b'\x00\x00\x00\x01' + socket.inet_aton(destination[0])
                flow['udp'].send(packet + struct.pack('!H', destination[1]) + data)
                if len(data) >= 20 and data[:2] == b'\x00\x01' and data[4:8] == bytes.fromhex('2112a442'):
                    log(direction='out', destination=destination, transaction=data[8:20].hex())
            else:
                entry = sockets.get(sock)
                if entry is None:
                    continue
                key, kind = entry
                if kind == 'control':
                    sock.recv(1024)
                    close(key)
                    continue
                try:
                    packet = sock.recv(65535)
                except OSError:
                    close(key)
                    continue
                if len(packet) < 10 or packet[:4] != b'\x00\x00\x00\x01':
                    log(drop='unsupported_relay_header')
                    continue
                origin = (socket.inet_ntoa(packet[4:8]), struct.unpack('!H', packet[8:10])[0])
                if origin != key[1]:
                    log(drop='unexpected_origin')
                    continue
                payload = packet[10:]
                flows[key]['listener'].sendto(payload, key[0])
                observed = stun_address(payload)
                if observed:
                    log(direction='return', receiver=origin, transaction=payload[8:20].hex(),
                        server_observed_ip=observed)
        for key, flow in list(flows.items()):
            if time.monotonic() - flow['last'] > 60:
                close(key)


if __name__ == '__main__':
    main()
