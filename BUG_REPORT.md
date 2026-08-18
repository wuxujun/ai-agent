# Bug Status Report — ai-agent

**Original audit:** 2026/06/22 · 18 confirmed findings

**Current verification:** 2026/08/18

**Method:** source review against every original finding, regression-test mapping,
full Go test suite, and `go vet`.

## Executive Summary

The original report is largely resolved:

- **17 of 18 original findings are fixed or effectively mitigated.**
- **#11 (`apply_patch` TOCTOU) is only partially fixed.** The final path
  component is protected on Unix, but ancestor-directory swaps and Windows
  reparse-point behavior remain unresolved.
- **Four current findings are tracked below:** two semaphore bugs, one shutdown
  classification bug, and the residual filesystem security issue.

The highest-impact original issues — approval bypass, stale approval dispatch,
`git_diff` argument injection, authentication fail-open, and redirect SSRF —
all have concrete fixes in the current tree.

## Current Open Findings

### 🟠 A. `apply_patch` remains vulnerable to ancestor-directory TOCTOU

**Location:** `internal/tools/apply_patch.go:156-199`,
`internal/policy/nofollow_windows.go`

`applySearchReplaceBlocks` validates the path, then opens the final file with
`O_NOFOLLOW`. This prevents following a symlink in the final path component on
Unix, but it does not prevent an ancestor directory from being replaced with a
symlink between validation and open:

```text
workspace/safe/file.txt
          ^ `safe` can be swapped after validation
```

Normal `open(path, O_NOFOLLOW)` may still follow symlinks in intermediate path
components. On Windows, the project defines `O_NOFOLLOW` as `0`, so the same
protection is not present for reparse points.

**Impact:** A process able to mutate the workspace concurrently may redirect a
patch write outside the validated workspace.

**Recommended fix:** On Linux, open relative to a trusted workspace directory
descriptor with `openat2` and `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS`, with a
carefully reviewed `openat` fallback where needed. Implement the corresponding
reparse-point-safe handle strategy on Windows. Add a regression test that swaps
an ancestor directory, not only a test where the final file is a symlink.

---

### 🟡 B. Resizable semaphore can leak capacity during waiter cancellation

**Location:** `internal/api/handler.go:1745-1790`

`wakeWaiters` increments `current` and closes a waiter's channel. If that
channel and `ctx.Done()` become ready at the same time, Go may select the
cancellation branch. The waiter has already been removed from `waiters`, so the
cancellation path cannot find it and does not return the granted reservation.

**Impact:** Each occurrence leaves `current` one too high. Repeated races can
eventually make the API reject or indefinitely queue work despite having free
execution capacity.

**Recommended fix:** Give each waiter an explicit pending/granted/cancelled
state under the semaphore lock. A cancelled waiter that was already granted
must release that grant before returning false. Add a deterministic regression
test for cancellation racing with wake-up.

---

### 🟡 C. Concurrency-limit reload can wake waiters with a stale limit

**Location:** `internal/api/handler.go:714-720, 980-993, 1745-1790`

Each caller passes its captured limit to both `Acquire` and `Release`. After a
hot reload reduces `max_concurrent_tasks`, a task acquired under the old limit
can later call `Release(oldLimit)`. Its `wakeWaiters(oldLimit)` may admit more
work than the new configuration permits.

**Impact:** A reduced concurrency limit is not reliably enforced until all
old-limit holders have exited; mixed callers can repeatedly wake waiters using
different limits.

**Recommended fix:** Store one authoritative limit inside the semaphore and
provide a synchronized `Resize(newLimit)` operation. `Acquire` and `Release`
should not accept independent limit values. Test both limit increase and limit
decrease while holders and waiters are active.

---

### ⚪ D. Shutdown rollback infers cancellation from final-answer length

**Location:** `internal/api/handler.go:270-300`

Shutdown rollback treats a failed task with an empty or short final answer as a
likely cancellation and transitions it to `paused`. A genuine failure such as
`timeout`, `invalid plan`, or `tool failed` may therefore be resurrected. A
long cancellation message may have the opposite result.

**Impact:** Task state after restart may not reflect the real terminal cause,
leading to unintended retries or missed resumptions.

**Recommended fix:** Persist a structured failure/termination kind and only
roll back explicit shutdown or cancellation failures. Do not classify failure
origin using human-readable text length.

## Original Findings — Current Status

| # | Original finding | Status | Current evidence |
|---:|---|---|---|
| 1 | Default Eino bypassed high-risk approval | **Fixed** | Shared `Engine.enforceApprovals` gates high-risk actions before execution; Eino and legacy/step call it, while ADK exposes confirmation requirements. Regression coverage exists in `internal/orchestrator/eino_test.go` and ADK tests. |
| 2 | Shutdown rollback raced live `run-all` | **Fixed** | Shutdown skips rollback for still-running reservations and uses `TryTransitionTaskStatus` CAS. Covered by `internal/api/graceful_shutdown_test.go`. Finding D above tracks a separate classification weakness. |
| 3 | Stale approval ID resolved a different approval | **Fixed** | `dispatchApproval` falls back by task ID only when `ApprovalID` is empty. Non-empty stale IDs are ignored. Covered by `TestApprovalBusNoFallbackOnNonEmptyApprovalID`. |
| 4 | `MockPlanner` indexed `Trace[2]` unsafely | **Fixed** | It bounds-checks the trace and falls back to the last trace. Covered by `TestMockPlannerNoPanicOnShortTrace`. |
| 5 | `git_diff` allowed option/argument injection | **Fixed** | Leading `-` is rejected, execution revalidates parameters, and `--` separates options from the path. Covered by `internal/tools/git_diff_test.go`. |
| 6 | `web_search` followed unsafe redirects | **Fixed** | It uses `policy.SafeHTTPClient`, whose tests cover private redirect and dial targets. |
| 7 | Missing API key disabled authentication | **Fixed** | Production API-key mode fails closed with 503 when no key is configured. Test-mode bypass remains explicitly limited to tests. |
| 8 | API key comparison was not constant-time | **Fixed** | `constantTimeKeyMatch` uses `subtle.ConstantTimeEq` and `subtle.ConstantTimeCompare`. |
| 9 | Responses API token usage remained zero | **Fixed** | Parser reads `input_tokens`/`output_tokens`, falls back to legacy names, and handles streamed usage. Covered by `provider_usage_test.go`. |
| 10 | Adaptive-depth loop inflated completion metrics | **Fixed** | Completion is incremented after the loop resolves rather than once per depth iteration. |
| 11 | `apply_patch` symlink TOCTOU | **Partially fixed** | Final-component symlinks are blocked with `O_NOFOLLOW` on Unix and tested, but ancestor swaps and Windows behavior remain open as finding A. |
| 12 | MemoryStore launched duplicate embedding jobs | **Fixed** | The indexing reservation is written synchronously before starting the goroutine. Covered by duplicate/index-gate tests. |
| 13 | Gemini stream discarded parts after `Parts[0]` | **Fixed** | Streaming code iterates every returned part. |
| 14 | Mixed embedding dimensions silently degraded RAG | **Mitigated** | Mismatches are counted/warned and retrieval falls back to keyword overlap. The system still does not persist a full embedding model identity, but the reported silent-zero behavior is removed. |
| 15 | Approval could be lost when context fired concurrently | **Fixed** | The context branch performs a non-blocking second receive before returning cancellation. |
| 16 | Ollama accepted an empty embedding | **Fixed** | Empty embedding responses return an error and can trigger fallback. |
| 17 | Concurrency capacity was cached at startup | **Partially superseded** | Request paths now read the hot configuration dynamically. Finding C identifies a new stale-release flaw in the replacement semaphore design. |
| 18 | Redis index migration used only a process mutex | **Fixed** | Migration uses a Redis `SETNX` lock plus completion marker and double-check. |

## Verification

Commands run during the 2026/08/18 review:

```bash
go test ./...
go test ./internal/multiagent
go vet ./...
```

The first full-suite run was inside a restricted sandbox. All packages passed
except `internal/multiagent`, where `httptest.NewServer` could not bind a local
ephemeral port (`operation not permitted`). Re-running that package with local
loopback listening allowed passed successfully. `go vet ./...` also passed.

These results validate the existing regression suite but do not prove absence
of races that require a specific interleaving, particularly findings B and C.

## Recommended Priority

1. **P0/P1 security:** close the ancestor-directory and Windows path-resolution
   gap in finding A.
2. **P1 reliability:** redesign the semaphore around explicit waiter state and
   a single authoritative limit, addressing B and C together.
3. **P2 correctness:** replace shutdown text heuristics with a structured
   termination reason.
4. Add regression tests for all three areas before marking this report clear.

## Current Risk Pattern

The project has moved beyond the original broad approval/authentication gaps.
The remaining risks concentrate in:

- filesystem resolution under concurrent mutation and across platforms;
- concurrency state transitions during cancellation and hot reload;
- durable task-state classification during shutdown and recovery.

Future bug hunts should prioritize these state-machine boundaries, plus
cross-instance approval, lifecycle-audit, Wiki/MCP, and hot-reload behavior.
