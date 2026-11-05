#!/usr/bin/env python3
"""Once-authorized target-kernel fixture: stdin only, no remote file or host namespace writes."""
from pathlib import Path
import subprocess
import sys

HERE = Path(__file__).resolve().parent
source = (HERE/'ingress_gate.py').read_text(encoding='utf-8')
fixture = (HERE/('fixture/diagnose_gate.py' if '--diagnose' in sys.argv else 'test_gate_netns.py')).read_text(encoding='utf-8')
payload = ('import sys,types\n'
           'module=types.ModuleType("ingress_gate")\n'
           'sys.modules[module.__name__]=module\n'
           f'exec(compile({source!r},"ingress_gate.py","exec"),module.__dict__)\n'
           f'exec(compile({fixture!r},"test_gate_netns.py","exec"))\n')
command = ('ARGON_NETNS_PARENT="$(readlink /proc/self/ns/net)" '
           'timeout --kill-after=2s 24s unshare --net --pid --fork --kill-child --mount-proc -- /usr/bin/python3 -B -')
subprocess.run(['ssh', '-T', '-F', 'C:/Users/xxoy1/.ssh/gcp-sub2api-migration-20261104/config-root',
                'gcp-task49-root', command], input=payload, text=True, check=True, timeout=33)
