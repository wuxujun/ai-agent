# Task 8 implementation report

- Added the snapshot-pinned Brain Provider with release verification and live retraction-watermark checks before and after reads.
- Added retrieval Brain scope options and corpus-aware Wiki tool schemas; disabled Brain preserves the existing schema and routing.
- Added bounded deterministic `all` corpus merging and tenant/task/project/snapshot cache isolation.
- Wired Brain corpus registration into the server Wiki builder only when Brain is enabled.

Verification passed for focused Brain/Tools race tests, server Wiki builder tests, vet, and diff-check. Broader package runs remain subject to the sandbox IPv6 `[::1]:0` httptest restriction.

## Review fix round

- Preserved corpus provenance through `all` candidate generation so Brain
  candidates fetch through the pinned Provider and watermark checks.
- Brain reads and graph operations now use the authorized project's Wiki space,
  and graph corpus routing is exposed conditionally.
- Added Brain/all corpus graph routing and stable scoped candidate/cache keys.
