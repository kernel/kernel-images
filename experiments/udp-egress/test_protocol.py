import socket
import struct
import threading
import unittest

from common import (datagram_capsule, decode_varint, encode_varint, open_tunnel,
                    parse_capsules, udp_targets)
from udp import stun_address


class ProtocolTests(unittest.TestCase):
    def egress(self, reply):
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen(1)
        self.addCleanup(listener.close)
        requests = []

        def serve():
            with listener.accept()[0] as connection:
                request = b''
                while b'\r\n\r\n' not in request:
                    request += connection.recv(4096)
                requests.append(request)
                connection.sendall(reply)

        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        settings = {'egress_host': '127.0.0.1', 'egress_port': listener.getsockname()[1]}
        return settings, thread, requests

    def test_tunnel(self):
        settings, thread, requests = self.egress(
            b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: connect-udp\r\n\r\n' + datagram_capsule(b'early'))
        sock, rest = open_tunnel(settings, ('8.8.8.8', 53))
        sock.close()
        thread.join(2)
        self.assertTrue(requests[0].startswith(b'GET /.well-known/masque/udp/8.8.8.8/53/ HTTP/1.1\r\n'))
        self.assertIn(b'Upgrade: connect-udp\r\n', requests[0])
        self.assertNotIn(b'Proxy-Authorization', requests[0])
        self.assertEqual(parse_capsules(rest), ([b'early'], b''))

    def test_tunnel_refused(self):
        settings, thread, _ = self.egress(b'HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n')
        with self.assertRaisesRegex(OSError, '403'):
            open_tunnel(settings, ('8.8.8.8', 443))
        thread.join(2)

    def test_varint(self):
        for value in [0, 63, 64, 16383, 16384, 2**30 - 1, 2**30, 2**62 - 1]:
            encoded = encode_varint(value)
            self.assertEqual(decode_varint(encoded), (value, len(encoded)))
            self.assertIsNone(decode_varint(encoded[:-1]) if len(encoded) > 1 else decode_varint(b''))

    def test_capsules(self):
        stream = datagram_capsule(b'one') + encode_varint(7) + encode_varint(2) + b'xx'
        stream += datagram_capsule(b'two')
        self.assertEqual(parse_capsules(stream), ([b'one', b'two'], b''))
        partial = datagram_capsule(b'three')
        self.assertEqual(parse_capsules(stream + partial[:3]), ([b'one', b'two'], partial[:3]))
        nonzero_context = encode_varint(0) + encode_varint(2) + b'\x01x'
        self.assertEqual(parse_capsules(nonzero_context), ([], b''))

    def test_explicit_targets(self):
        valid = [{'host': '8.8.8.8', 'port': 3478}]
        self.assertEqual(udp_targets({'udp_targets': valid}), valid)
        invalid = [[], valid * 17, valid * 2,
                   [{'host': '127.0.0.1', 'port': 3478}],
                   [{'host': '2001:4860:4860::8888', 'port': 3478}],
                   [{'host': '8.8.8.8', 'port': 0}],
                   [{'host': '8.8.8.8', 'port': True}]]
        for targets in invalid:
            with self.subTest(targets=targets), self.assertRaises(ValueError):
                udp_targets({'udp_targets': targets})

    def test_stun_address(self):
        ip = int.from_bytes(socket.inet_aton('198.51.100.25'), 'big') ^ 0x2112a442
        value = struct.pack('!BBHI', 0, 1, 50000 ^ 0x2112, ip)
        response = bytes.fromhex('0101000c2112a442') + b'abcdefghijkl'
        response += struct.pack('!HH', 32, 8) + value
        self.assertEqual(stun_address(response), '198.51.100.25')
        self.assertIsNone(stun_address(response[:-1]))
        self.assertIsNone(stun_address(b'not a STUN packet'))


if __name__ == '__main__':
    unittest.main()
