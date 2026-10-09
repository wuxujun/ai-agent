#!/usr/bin/env python3
"""Require all seven external contracts to pass, without skipped subtests."""
import json
import sys

EXPECTED = {
    "TestExternalStoresTaskCreation", "TestExternalStoresTaskLeaseGuard",
    "TestExternalStoresPersistPausedTaskAcrossClients", "TestExternalPostgresDurableApprovalCASAcrossClients",
    "TestExternalPostgresDurableApprovalRecoveryContract", "TestPostgresPoolExternal", "TestPostgresPoolHAExternal",
}


def verify(events, exit_code):
    passed, failed, skipped = set(), set(), set()
    for event in events:
        test, action = event.get("Test"), event.get("Action")
        if action == "fail":
            failed.add(test or "package")
        if test and action == "skip":
            skipped.add(test)
        if test and "/" not in test and action == "pass":
            passed.add(test)
    ok = exit_code == 0 and EXPECTED <= passed and not failed and not skipped
    return {"passed": ok, "required_top_level": 7, "observed_top_level": sorted(passed),
        "missing": sorted(EXPECTED - passed), "failed": sorted(failed), "skipped": sorted(skipped),
        "full_ha_acceptance": False}


if __name__ == "__main__":
    try:
        with open(sys.argv[1], encoding="utf-8") as stream:
            report = verify((json.loads(line) for line in stream if line.strip()), int(sys.argv[2]))
        print(json.dumps(report, indent=2))
        sys.exit(0 if report["passed"] else 1)
    except (OSError, ValueError, IndexError):
        raise SystemExit(2)
