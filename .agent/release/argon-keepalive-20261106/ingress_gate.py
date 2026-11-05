"""Per-run nft table. Only this table is ever created or deleted."""
import json
import re
import subprocess


class GateError(Exception):
    pass


def need(value, message):
    if not value:
        raise GateError(message)


def table_name(run):
    need(re.fullmatch(r'run-[0-9]{8}T[0-9]{6}Z', run), 'invalid_gate_run')
    return 'sub2_' + run[4:].replace('T', '_')[:-1]


def match(left, right):
    return {'match': {'op': '==', 'left': left, 'right': right}}


def objects(name, bridge, uid):
    need(re.fullmatch(r'br-[0-9a-f]{12}', bridge) and uid == 33, 'gate_bridge_or_nginx_uid_changed')
    result = []
    initial_syn = match({'&': [{'payload': {'protocol': 'tcp', 'field': 'flags'}}, 23]}, 2)
    for family, chain, hook, priority, expressions in (
        ('inet', 'nginx_out', 'output', -101, [match({'payload': {'protocol': 'ip', 'field': 'daddr'}}, '127.0.0.1'),
                                              match({'meta': {'key': 'skuid'}}, uid),
                                              match({'payload': {'protocol': 'tcp', 'field': 'dport'}}, 17861)]),
        ('bridge', 'internal_forward', 'forward', -1, [match({'meta': {'key': 'ibrname'}}, bridge),
                                                      match({'meta': {'key': 'obrname'}}, bridge),
                                                      match({'meta': {'key': 'l4proto'}}, 6),
                                                      match({'payload': {'protocol': 'tcp', 'field': 'dport'}}, 8080)])):
        result.append({'table': {'family': family, 'name': name}})
        result.append({'chain': {'family': family, 'table': name, 'name': chain, 'type': 'filter', 'hook': hook,
                                 'prio': priority, 'policy': 'accept'}})
        verdict = {'drop': None} if family == 'bridge' else {'reject': {'type': 'tcp reset'}}
        result.append({'rule': {'family': family, 'table': name, 'chain': chain, 'comment': 'sub2-release:' + name,
                                'expr': expressions + [initial_syn, {'counter': {'packets': 0, 'bytes': 0}}, verdict]}})
    return result


def normalized(value):
    if isinstance(value, list):
        # nft 1.0.6 elides the TCP payload's implicit l4proto dependency.
        tcp_payload = any(row.get('match', {}).get('left', {}).get('payload', {}).get('protocol') == 'tcp' for row in value if isinstance(row, dict))
        dependency = match({'meta': {'key': 'l4proto'}}, 6)
        return [normalized(item) for item in value if not (tcp_payload and item == dependency)]
    if isinstance(value, dict):
        if set(value) == {'counter'}:
            return {'counter': {}}
        if set(value) == {'&'} and isinstance(value['&'][1], list):
            mask = 0
            for bit in value['&'][1]:
                mask |= bit
            return {'&': [normalized(value['&'][0]), mask]}
        if set(value) == {'|'}:
            result = 0
            for item in value['|']:
                result |= item
            return result
        return {key: normalized(item) for key, item in value.items() if key not in ('handle', 'index')}
    return value


class Gate:
    def __init__(self, run, facts):
        self.name = table_name(run)
        self.expected = objects(self.name, facts['bridge'], facts['nginx_uid'])

    def nft(self, arguments, document=None):
        result = subprocess.run(['nft', '-j', '-n', *arguments],
                                input=json.dumps(document) if document is not None else None,
                                capture_output=True, text=True, timeout=10)
        need(result.returncode == 0, 'nft_command_failed_' + str(result.returncode) + ':' + result.stderr.strip()[:2000])
        return json.loads(result.stdout) if result.stdout.strip() else {}

    def present(self):
        value = self.nft(['list', 'tables'])
        return {row['table']['family'] for row in value['nftables']
                if 'table' in row and row['table']['name'] == self.name and row['table']['family'] in ('inet', 'bridge')}

    def verify(self):
        observed, counters = [], {}
        need(self.present() == {'inet', 'bridge'}, 'owned_gate_incomplete')
        for family in ('inet', 'bridge'):
            value = self.nft(['list', 'table', family, self.name])
            observed.extend(normalized(row) for row in value['nftables'] if 'metainfo' not in row)
            counters.update({row['rule']['chain']: next(expr['counter']['packets'] for expr in row['rule']['expr'] if 'counter' in expr)
                             for row in value['nftables'] if 'rule' in row})
        key = lambda row: json.dumps(row, sort_keys=True)
        need(sorted(observed, key=key) == sorted([normalized(row) for row in self.expected], key=key), 'owned_gate_rule_drift')
        return counters

    def document(self):
        return {'nftables': [{('create' if 'table' in item else 'add'): item} for item in self.expected]}

    def preflight(self):
        rows = self.nft(['list', 'tables'])['nftables']
        need(not any(re.fullmatch(r'sub2_[0-9]{8}_[0-9]{6}', row.get('table', {}).get('name', ''))
                     and row['table']['family'] in ('inet', 'bridge') for row in rows), 'another_sub2_gate_is_active')
        self.nft(['--check', '--file', '-'], self.document())

    def install(self):
        self.preflight()
        # One netlink transaction: both families are installed atomically.
        self.nft(['--file', '-'], self.document())
        return self.verify()

    def remove(self):
        if self.present():
            self.verify()
            self.nft(['--file', '-'], {'nftables': [{'delete': item} for item in self.expected if 'table' in item]})
        need(not self.present(), 'owned_gate_not_removed')
