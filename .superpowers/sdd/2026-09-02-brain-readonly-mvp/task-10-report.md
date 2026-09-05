# Task 10 report: Brain observability and readiness

Implemented bounded Brain lifecycle metrics through `brain.CurrentMetrics()` and
sanitized Brain status in `/ready` and `/api/metrics`. Disabled Brain reports
`configured=false` and never affects readiness; empty CURRENT is reportable and
does not block ordinary traffic. Invalid or unreadable Brain roots fail during
initialization.

Operations remain CLI-only: use explicit tenant/project arguments, review and
verify staging before CAS publish, and provide expected CURRENT for publish or
rollback. Exit codes are 0 success, 1 controlled validation/provider failure,
and 2 usage/configuration failure. Retraction is logical blocking, not physical
erasure; `brain.root` changes require restart; no HTTP write route exists.

Verification: focused Brain/API tests, vet, and `git diff --check` passed.
