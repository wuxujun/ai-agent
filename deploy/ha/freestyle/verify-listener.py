#!/usr/bin/env python3
"""Require TCP port 2049 to listen only on the intended private IPv4 address."""
import ipaddress
import sys


def listening_only_at(text, expected):
    expected = ipaddress.IPv4Address(expected)
    listeners = []
    for line in text.splitlines():
        fields = line.split()
        if not fields or fields[0] != 'LISTEN' or len(fields) < 4:
            continue
        address, separator, port = fields[3].rpartition(':')
        if separator and port == '2049':
            try:
                value = ipaddress.ip_address(address.strip('[]'))
            except ValueError:
                return False
            if isinstance(value, ipaddress.IPv6Address):
                value = value.ipv4_mapped or value
            listeners.append(value)
    return bool(listeners) and all(value == expected for value in listeners)


if __name__ == '__main__':
    try:
        passed = len(sys.argv) == 2 and listening_only_at(sys.stdin.read(), sys.argv[1])
    except ValueError:
        passed = False
    raise SystemExit(0 if passed else 2)
