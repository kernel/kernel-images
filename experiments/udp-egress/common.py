import ipaddress
import json
import socket
import struct


def config():
    with open('/run/udp-egress/upstream.json') as f:
        return json.load(f)


def udp_targets(settings):
    targets = settings['udp_targets']
    if not isinstance(targets, list) or not 0 < len(targets) <= 16:
        raise ValueError('configure between 1 and 16 explicit UDP targets')
    seen = set()
    for target in targets:
        address = ipaddress.ip_address(target['host'])
        port = target['port']
        if address.version != 4 or not address.is_global:
            raise ValueError('UDP targets must be public IPv4 addresses')
        if type(port) is not int or not 1 <= port <= 65535:
            raise ValueError('invalid UDP target port')
        endpoint = (str(address), port)
        if endpoint in seen:
            raise ValueError('duplicate UDP target')
        seen.add(endpoint)
    return targets


def receive(sock, count):
    result = b''
    while len(result) < count:
        part = sock.recv(count - len(result))
        if not part:
            raise OSError('SOCKS control connection closed')
        result += part
    return result


def associate(settings, port):
    sock = socket.create_connection((settings['host'], settings['socks_port']), timeout=8)
    try:
        sock.sendall(b'\x05\x01\x02')
        if receive(sock, 2) != b'\x05\x02':
            raise OSError('SOCKS username/password authentication unavailable')
        user, password = settings['user'].encode(), settings['password'].encode()
        if not 0 < len(user) < 256 or not 0 < len(password) < 256:
            raise ValueError('invalid SOCKS credential length')
        sock.sendall(b'\x01' + bytes([len(user)]) + user + bytes([len(password)]) + password)
        if receive(sock, 2) != b'\x01\x00':
            raise OSError('SOCKS authentication failed')
        sock.sendall(b'\x05\x03\x00\x01\x00\x00\x00\x00' + struct.pack('!H', port))
        version, status, reserved, address_type = receive(sock, 4)
        if version != 5 or status != 0 or reserved != 0 or address_type != 1:
            raise OSError('SOCKS UDP association requires a successful IPv4 reply')
        host = socket.inet_ntoa(receive(sock, 4))
        bound_port = struct.unpack('!H', receive(sock, 2))[0]
        if host == '0.0.0.0':
            host = settings['host']
        if host != settings['host'] or bound_port == 0:
            raise OSError('unexpected SOCKS relay endpoint')
        return sock, (host, bound_port)
    except Exception:
        sock.close()
        raise
