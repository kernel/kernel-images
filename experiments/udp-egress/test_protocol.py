import socket
import struct
import threading
import unittest

from common import associate, receive, udp_targets
from udp import stun_address


class ProtocolTests(unittest.TestCase):
    def proxy(self, auth_status=0, command_status=0):
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen(1)
        self.addCleanup(listener.close)
        errors = []

        def serve():
            try:
                with listener.accept()[0] as connection:
                    self.assertEqual(receive(connection, 3), b'\x05\x01\x02')
                    connection.sendall(b'\x05\x02')
                    self.assertEqual(receive(connection, 1), b'\x01')
                    self.assertEqual(receive(connection, receive(connection, 1)[0]), b'user')
                    self.assertEqual(receive(connection, receive(connection, 1)[0]), b'password')
                    connection.sendall(bytes([1, auth_status]))
                    if auth_status:
                        return
                    self.assertEqual(receive(connection, 10), bytes.fromhex('05030001000000003039'))
                    connection.sendall(bytes([5, command_status, 0, 1]) + socket.inet_aton('127.0.0.1') + struct.pack('!H', 45678))
            except Exception as error:
                errors.append(error)

        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        settings = {'host': '127.0.0.1', 'socks_port': listener.getsockname()[1],
                    'user': 'user', 'password': 'password'}
        return settings, thread, errors

    def test_association(self):
        settings, thread, errors = self.proxy()
        control, relay = associate(settings, 12345)
        control.close()
        thread.join(2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(relay, ('127.0.0.1', 45678))

    def test_authentication_failure(self):
        settings, thread, errors = self.proxy(auth_status=1)
        with self.assertRaisesRegex(OSError, 'authentication failed'):
            associate(settings, 12345)
        thread.join(2)
        self.assertEqual(errors, [])

    def test_command_failure(self):
        settings, thread, errors = self.proxy(command_status=7)
        with self.assertRaisesRegex(OSError, 'successful IPv4 reply'):
            associate(settings, 12345)
        thread.join(2)
        self.assertEqual(errors, [])

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
