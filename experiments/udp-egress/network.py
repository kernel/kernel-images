import pathlib
import subprocess
import time

from common import config, udp_targets


def run(*args):
    subprocess.run(args, check=True)


def main():
    ready = pathlib.Path('/run/udp-egress-ready')
    ready.unlink(missing_ok=True)
    run('ip', 'netns', 'add', 'browser-udp')
    run('ip', 'link', 'add', 'udp-host', 'type', 'veth', 'peer', 'name', 'udp-browser')
    run('ip', 'link', 'set', 'udp-browser', 'netns', 'browser-udp')
    run('iptables', '-I', 'FORWARD', '1', '-i', 'udp-host', '-j', 'DROP')
    targets = udp_targets(config())
    for index, target in enumerate(targets):
        run('iptables', '-t', 'nat', '-A', 'PREROUTING', '-i', 'udp-host', '-p', 'udp',
            '-d', target['host'], '--dport', str(target['port']), '-j', 'DNAT',
            '--to-destination', f'192.0.2.1:{40000 + index}')
    run('ip', 'addr', 'add', '192.0.2.1/30', 'dev', 'udp-host')
    run('ip', 'link', 'set', 'udp-host', 'up')
    prefix = ('ip', 'netns', 'exec', 'browser-udp')
    if pathlib.Path('/proc/sys/net/ipv6').exists():
        run(*prefix, 'sysctl', '-w', 'net.ipv6.conf.all.disable_ipv6=1',
            'net.ipv6.conf.default.disable_ipv6=1')
    run(*prefix, 'ip', 'link', 'set', 'lo', 'up')
    run(*prefix, 'ip', 'addr', 'add', '192.0.2.2/30', 'dev', 'udp-browser')
    run(*prefix, 'ip', 'link', 'set', 'udp-browser', 'up')
    run(*prefix, 'ip', 'route', 'add', 'default', 'via', '192.0.2.1')
    resolv = pathlib.Path('/etc/netns/browser-udp')
    resolv.mkdir(parents=True)
    (resolv / 'resolv.conf').write_text('nameserver 8.8.8.8\n')

    processes = [
        subprocess.Popen(['python3', '-u', '/opt/udp-egress/http_proxy.py']),
        subprocess.Popen(['socat', 'TCP4-LISTEN:9223,bind=127.0.0.1,reuseaddr,fork',
                          'TCP4:192.0.2.2:9223']),
        subprocess.Popen([*prefix, 'socat',
                          'TCP4-LISTEN:9223,bind=192.0.2.2,reuseaddr,fork',
                          'TCP4:127.0.0.1:9223']),
    ]
    time.sleep(1)
    if any(p.poll() is not None for p in processes):
        raise RuntimeError('network helper failed during startup')
    ready.touch()
    print('browser namespace ready; forwarding outside the relay is denied', flush=True)
    while True:
        for p in processes:
            if p.poll() is not None:
                ready.unlink(missing_ok=True)
                raise RuntimeError('network helper exited; namespace forwarding remains denied')
        time.sleep(1)


if __name__ == '__main__':
    main()
