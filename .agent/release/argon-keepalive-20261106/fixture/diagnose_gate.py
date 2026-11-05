"""Differentiate unsupported native bridge expressions in a throwaway netns."""
import json
import os
from pathlib import Path
import subprocess

assert os.readlink('/proc/self/ns/net') != os.environ['ARGON_NETNS_PARENT']
assert {row['ifname'] for row in json.loads(subprocess.check_output(['ip','-j','link']))} == {'lo'}
config = Path('/boot/config-' + os.uname().release)
if config.is_file():
    print('\n'.join(line for line in config.read_text().splitlines() if any(key in line for key in ('NF_TABLES_BRIDGE', 'NFT_BRIDGE_META', 'NFT_BRIDGE_REJECT', 'NFT_REJECT'))), flush=True)
print(sorted(path.name for path in Path('/lib/modules', os.uname().release, 'kernel/net/bridge/netfilter').glob('nft*.ko*')), flush=True)
bridge = 'create table bridge probe\nadd chain bridge probe f { type filter hook forward priority -1; policy accept; }\n'
inet = 'create table inet probe\nadd chain inet probe f { type filter hook output priority -101; policy accept; }\n'
syn = 'tcp flags & (fin|syn|rst|ack) == syn'
for name, script in (
    ('inet_output_reset', inet + f'add rule inet probe f ip daddr 127.0.0.1 meta skuid 33 tcp dport 17861 {syn} counter reject with tcp reset'),
    ('bridge_tcp_ipv4_drop', bridge + f'add rule bridge probe f ether type ip ip protocol tcp tcp dport 8080 {syn} counter drop'),
    ('bridge_tcp_l4proto_drop', bridge + f'add rule bridge probe f meta l4proto tcp tcp dport 8080 {syn} counter drop'),
    ('bridge_ibr_obr_drop', bridge + f'add rule bridge probe f meta ibrname "br-0123456789ab" meta obrname "br-0123456789ab" meta l4proto tcp tcp dport 8080 {syn} counter drop'),
    ('bridge_ibr_obr_reset', bridge + f'add rule bridge probe f meta ibrname "br-0123456789ab" meta obrname "br-0123456789ab" meta l4proto tcp tcp dport 8080 {syn} counter reject with tcp reset'),
):
    result = subprocess.run(['nft', '-c', '-f', '-'], input=script + '\n', text=True, capture_output=True, timeout=3)
    print(json.dumps({'case': name, 'rc': result.returncode, 'stderr': result.stderr.strip()}), flush=True)
