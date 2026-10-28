import base64
import selectors
import socket
import socketserver

from common import config

SETTINGS = config()


class Proxy(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(30)
        header = b''
        while not header.endswith(b'\r\n\r\n'):
            part = self.request.recv(1)
            if not part:
                return
            header += part
            if len(header) > 16384:
                return
        lines = header[:-4].split(b'\r\n')
        lines = [line for line in lines if not line.lower().startswith(b'proxy-authorization:')]
        credentials = base64.b64encode((SETTINGS['user'] + ':' + SETTINGS['password']).encode())
        lines.append(b'Proxy-Authorization: Basic ' + credentials)
        with socket.create_connection((SETTINGS['host'], SETTINGS['http_port']), timeout=8) as upstream:
            upstream.settimeout(30)
            upstream.sendall(b'\r\n'.join(lines) + b'\r\n\r\n')
            with selectors.DefaultSelector() as selector:
                selector.register(self.request, selectors.EVENT_READ, upstream)
                selector.register(upstream, selectors.EVENT_READ, self.request)
                while True:
                    events = selector.select(timeout=30)
                    if not events:
                        return
                    for event, _ in events:
                        data = event.fileobj.recv(65536)
                        if not data:
                            return
                        event.data.sendall(data)


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if __name__ == '__main__':
    with Server(('192.0.2.1', 18080), Proxy) as server:
        print('HTTP forwarding ready', flush=True)
        server.serve_forever()
