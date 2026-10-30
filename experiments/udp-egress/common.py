import ipaddress
import json
import socket


def config():
    with open('/run/udp-egress/egress.json') as f:
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


def encode_varint(value):
    for size, prefix in ((1, 0), (2, 0x40), (4, 0x80), (8, 0xc0)):
        if value < 1 << (8 * size - 2):
            data = bytearray(value.to_bytes(size, 'big'))
            data[0] |= prefix
            return bytes(data)
    raise ValueError('varint too large')


def decode_varint(data, offset=0):
    """Returns (value, next offset), or None when data is incomplete."""
    if offset >= len(data):
        return None
    size = 1 << (data[offset] >> 6)
    if offset + size > len(data):
        return None
    value = data[offset] & 0x3f
    for byte in data[offset + 1:offset + size]:
        value = value << 8 | byte
    return value, offset + size


def datagram_capsule(payload):
    return encode_varint(0) + encode_varint(len(payload) + 1) + b'\x00' + payload


def parse_capsules(buffer):
    """Splits complete capsules off buffer. Returns (datagram payloads, rest)."""
    payloads = []
    offset = 0
    while True:
        kind = decode_varint(buffer, offset)
        if kind is None:
            break
        length = decode_varint(buffer, kind[1])
        if length is None or length[1] + length[0] > len(buffer):
            break
        body = buffer[length[1]:length[1] + length[0]]
        offset = length[1] + length[0]
        context = decode_varint(body)
        if kind[0] == 0 and context is not None and context[0] == 0:
            payloads.append(body[context[1]:])
    return payloads, buffer[offset:]


def open_tunnel(settings, destination):
    """Opens a connect-udp tunnel to destination through egress-proxy."""
    sock = socket.create_connection((settings['egress_host'], settings['egress_port']), timeout=10)
    try:
        host, port = destination
        sock.sendall((f'GET /.well-known/masque/udp/{host}/{port}/ HTTP/1.1\r\n'
                      f'Host: {settings["egress_host"]}:{settings["egress_port"]}\r\n'
                      'Connection: Upgrade\r\nUpgrade: connect-udp\r\n'
                      'Capsule-Protocol: ?1\r\n\r\n').encode())
        response = b''
        while b'\r\n\r\n' not in response:
            part = sock.recv(4096)
            if not part or len(response) > 16384:
                raise OSError('egress closed during connect-udp')
            response += part
        head, rest = response.split(b'\r\n\r\n', 1)
        status = head.split(b'\r\n', 1)[0]
        if status.split(b' ')[1:2] != [b'101']:
            raise OSError(f'connect-udp refused: {status.decode(errors="replace")}')
        sock.settimeout(None)
        return sock, rest
    except Exception:
        sock.close()
        raise
