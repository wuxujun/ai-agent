#!/usr/bin/env python3
"""Write/read a unique shared-volume sentinel as ai-agent; retain all probes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=['write', 'read'], required=True)
    parser.add_argument('--id', required=True)
    parser.add_argument('--sha256')
    args = parser.parse_args()
    if os.getuid() != 21088 or not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', args.id) or len(args.id) > 64:
        raise SystemExit('Run as the dedicated uid 21088 and use a bounded probe ID')
    root = Path('/opt/ai-agent/workspace/ha-probes')
    os.umask(0o077)
    if args.mode == 'write':
        root.mkdir(mode=0o750, exist_ok=True)
        path = root / (args.id + '.bin')
        with open(path, 'xb') as stream:
            stream.write(secrets.token_bytes(64))
            stream.flush()
            os.fsync(stream.fileno())
    else:
        if not args.sha256 or not re.fullmatch('[a-f0-9]{64}', args.sha256):
            raise SystemExit('--sha256 is required for read')
        path = root / (args.id + '.bin')
    if path.is_symlink() or path.stat().st_size != 64:
        raise SystemExit('Invalid sentinel')
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    if args.mode == 'read' and digest != args.sha256:
        raise SystemExit('Shared sentinel mismatch')
    print(json.dumps(dict(probe=args.id, mode=args.mode, sha256=digest, passed=True, full_ha_acceptance=False)))
