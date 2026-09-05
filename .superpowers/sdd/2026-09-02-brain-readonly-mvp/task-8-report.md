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

## Final review fix

- Server now registers a composite ordinary/Brain corpus router, enabling real
  `corpus=all` search and provenance-aware fetches.
- Brain graph requests bypass ordinary tenant-space validation and resolve the
  authorized project Wiki space; `corpus=all` graph is explicitly rejected.
- The server-backed corpus adapter refreshes the live retraction watermark on
  search, fetch, and graph operations, and the watermark participates in task
  cache keys.

## Final graph-fetch fix

- `wiki_graph_fetch` now preserves Brain provenance and reads Brain neighbors
  through `ReadCorpus`, including live watermark refresh, without ordinary
  tenant-space validation.
