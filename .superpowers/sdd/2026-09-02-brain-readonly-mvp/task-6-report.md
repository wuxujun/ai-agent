# Task 6 — Bounded Gemini Synthesis and Compiler Orchestration

## Status

Implemented the bounded Brain compiler. It reads the existing sanitized source
snapshot, uses only the configured Gemini credential through the existing
structured LLM runtime, validates all deterministic gates, and can create only
a staging snapshot. `Build` has no publication path.

## Files

- `internal/brain/compiler.go` — `BuildRequest`, `Compiler`, test seam,
  frozen Gemini LLM configuration, bounded request/schema construction,
  pre/post-call cost and token gates, source/retraction gates, deterministic
  synthesis validation, manifest provenance hashes, validation, and staging.
- `internal/brain/compiler_test.go` — local fake structured caller/source
  store/retraction ledger tests for the compiler trust boundary.
- `internal/config/config.go` — enabled Brain now requires positive compact
  index, compiler input-byte, and compiler output-token limits.
- `internal/config/config_test.go` — regression coverage for those enabled
  compiler bounds.

## TDD evidence

Initial RED command:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain -run 'TestCompiler'
```

Initial RED output:

```text
# github.com/wuxujun/ai-agent/internal/brain [github.com/wuxujun/ai-agent/internal/brain.test]
internal/brain/compiler_test.go:75:21: undefined: ErrCompileBudget
internal/brain/compiler_test.go:85:21: undefined: ErrCompileConfiguration
internal/brain/compiler_test.go:95:21: undefined: ErrCompileBudget
internal/brain/compiler_test.go:105:21: undefined: ErrCompileBudget
internal/brain/compiler_test.go:117:21: undefined: ErrCompileSynthesis
internal/brain/compiler_test.go:129:21: undefined: ErrCompileValidation
internal/brain/compiler_test.go:165:42: undefined: brainCompilerScene
internal/brain/compiler_test.go:198:91: undefined: Compiler
internal/brain/compiler_test.go:240:29: undefined: BuildRequest
FAIL github.com/wuxujun/ai-agent/internal/brain [build failed]
FAIL
```

The enabled-config limit regression was also written before its production
change:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/config -run '^TestValidateEnabledBrainRequiresPositiveCompilerBounds$' -count=1
--- FAIL: TestValidateEnabledBrainRequiresPositiveCompilerBounds (0.00s)
    config_test.go:182: enabled Brain accepted an unbounded compiler: {Enabled:true Root:./data/brain CompactIndexMaxBytes:0 Compiler:{Provider:gemini Model:gemini-3.5-flash-lite MaxInputBytes:200000 MaxOutputTokens:12000 MaxCostUSD:0.25}}
FAIL
FAIL github.com/wuxujun/ai-agent/internal/config
```

After the minimal compiler and configuration implementation, the focused GREEN
run was:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain -run 'TestCompiler' -count=1
ok   github.com/wuxujun/ai-agent/internal/brain
```

The final focused/package verification was:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain -run 'TestCompiler' -count=1
ok   github.com/wuxujun/ai-agent/internal/brain

GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain ./internal/config -count=1
ok   github.com/wuxujun/ai-agent/internal/brain
ok   github.com/wuxujun/ai-agent/internal/config

GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go vet ./...
exit 0

git diff --check
exit 0
```

Compiler tests use `llm.NewRuntime` with a local fake structured caller. They
do not read `GEMINI_API_KEY` and make no provider request.

## Coverage and self-review

- Oversize input is rejected before a model call; the conservative structured
  request token/transport upper bound is checked against the immutable Brain
  input limit.
- The compiler accepts only `LLM.GeminiAPIKey`, pins scene `brain_compiler`,
  provider `gemini`, the configured model, an output-token cap, a timeout, and
  no fallback configuration. It calls `Runtime.CallJSONExact`.
- The JSON schema is closed at root/page/claim levels and bounds page, claim,
  string, link, and evidence-reference shapes. Evidence references are an enum
  from the exact sanitized source IDs. Deterministic checks repeat all material
  trust-boundary checks after the untrusted structured output is decoded.
- Input source content is sanitized/bounded by `SourceReader` and checked again
  for deterministic prompt-injection or secret indicators before synthesis.
  Generated content is rendered then sent through the existing deterministic
  validator, including its injection/secret, provenance, link, and retraction
  gates.
- The compiler reads the watermark before model input, relies on validation's
  watermark check, and reads it again immediately before `CreateStage`; drift
  returns `ErrRetractionChanged` without staging.
- The staged manifest records source cutoff/IDs/hashes, model, prompt digest,
  non-secret config digest, actual usage, estimated cost, and watermark. It
  contains neither prompts, raw model response, credential, private path, nor
  full evidence body. The test serializes the manifest to assert this boundary.
- The success test confirms `CURRENT` remains empty while a staging snapshot is
  created, proving this compiler cannot publish.

## Verification concern

One allowed full-suite attempt was run:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./...
```

It reached the Brain/config packages and many unrelated packages, but exited
non-zero because this filesystem/network sandbox denies `httptest` from binding
`[::1]:0`. The observed unrelated panics were in `cmd/server`,
`internal/multiagent`, `internal/promptmanager`, and `internal/tools`; each
reported `listen tcp6 [::1]:0: bind: operation not permitted`. This matches the
known sandbox loopback limitation, not a Brain compiler assertion failure.
No live Gemini call was attempted.

## Fix round 1 — compiler boundary hardening

### Review findings resolved

- Compiler pricing now fails closed before source/model work unless both input
  and output prices are explicit, finite, non-negative, and at least one is
  non-zero. Enabled Brain configuration enforces the same rule. Reload
  fixtures now declare their compiler pricing explicitly.
- `llm.Config.StrictJSONSchema` is opt-in and set only for the Brain compiler.
  The native Gemini boundary now parses the complete response once, rejects
  duplicate JSON object fields, recursively applies object closure, required
  fields, string/pattern/enum bounds, array bounds, and `uniqueItems`, then
  uses `DisallowUnknownFields` before destination decoding. The zero value
  preserves all existing non-Brain callers' permissive behavior.
- Structured parse failures carry `llm.ErrStructuredOutput` without response
  text. The compiler maps that sentinel to `ErrCompileSynthesis`; source,
  provider, and repository failures map to sanitized stable categories
  (`ErrCompileInfrastructure` plus `ErrCompileSource`, `ErrCompileLLM`, or
  `ErrCompileRepository`). Repository failure also retains `ErrCompileStage`.
- Source/discovery content is rejected before `CallJSONExact` when it contains
  a conservative private-filesystem marker. Rendered/generated content is
  rejected by validation before staging with a generic `private_path` finding.
  Neither error nor manifest records the path.
- Usage now requires `total_tokens == prompt_tokens + completion_tokens`;
  duplicate page links are rejected deterministically; and prompt digest input
  now includes a stable template version and the closed response schema.

### Files changed in this round

- `internal/brain/compiler.go`, `internal/brain/compiler_test.go`
- `internal/brain/validate.go`
- `internal/config/config.go`, `internal/config/config_test.go`
- `internal/llm/structured.go`, `internal/llm/transport.go`,
  `internal/llm/transport_test.go`

### RED evidence

The new compiler pricing/category/usage/private-path/digest/schema tests and
strict LLM parser tests were written before the production changes. The exact
initial command was:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain ./internal/llm -run 'Test(Compiler|ParseStructuredJSONStrict)' -count=1
```

Its RED output was:

```text
# github.com/wuxujun/ai-agent/internal/llm [github.com/wuxujun/ai-agent/internal/llm.test]
internal/llm/transport_test.go:133:14: undefined: parseStructuredJSONStrict
internal/llm/transport_test.go:152:12: undefined: parseStructuredJSONStrict
# github.com/wuxujun/ai-agent/internal/brain [github.com/wuxujun/ai-agent/internal/brain.test]
internal/brain/compiler_test.go:223:10: undefined: ErrCompileSource
internal/brain/compiler_test.go:230:11: undefined: ErrCompileLLM
internal/brain/compiler_test.go:238:11: undefined: ErrCompileRepository
internal/brain/compiler_test.go:249:23: undefined: ErrCompileInfrastructure
internal/brain/compiler_test.go:287:177: fake.cfg.StrictJSONSchema undefined (type llm.Config has no field or method StrictJSONSchema)
internal/brain/compiler_test.go:487:45: too many arguments in call to compilerPromptDigest
	have (string, map[string]any)
	want (string)
internal/brain/compiler_test.go:489:48: too many arguments in call to compilerPromptDigest
	have (string, map[string]any)
	want (string)
FAIL	github.com/wuxujun/ai-agent/internal/brain [build failed]
FAIL
```

### GREEN and final verification

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain -run 'TestCompiler' -count=1
ok   github.com/wuxujun/ai-agent/internal/brain

GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/brain ./internal/config -count=1
ok   github.com/wuxujun/ai-agent/internal/brain
ok   github.com/wuxujun/ai-agent/internal/config

GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/llm -run 'TestParseStructuredJSONStrict|TestParseStructuredJSONFallback' -count=1
ok   github.com/wuxujun/ai-agent/internal/llm

GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go vet ./internal/brain ./internal/config ./internal/llm
exit 0

git diff --check
exit 0
```

The strict parser tests directly exercise the LLM transport boundary. A second
local-only native Gemini transport test uses an `httptest` fake response with a
nested undeclared field. Its attempted command was:

```text
GOCACHE=/private/tmp/ai-agent-brain-mvp-go-cache go test ./internal/llm -run 'TestNativeGeminiStrictSchemaRejectsNestedUnknownResponse|TestParseStructuredJSONStrict' -count=1
```

It was blocked before the handler ran by this sandbox's loopback restriction:
`httptest: failed to listen on a port: listen tcp6 [::1]:0: bind: operation not permitted`.
No Gemini provider request or credential was used. The elevated local-test
rerun was user-aborted, so this is an environment limitation, not a code/test
assertion failure. The package's existing `httptest` tests have the same
restriction here; no additional full-suite run was made.

### Fix-round self-review and remaining concern

The native Gemini SDK cannot represent `additionalProperties` or
`uniqueItems`; strict validation therefore runs immediately after the Gemini
response and before typed decode. Schema changes alter the manifest prompt
digest without storing schema, prompts, or response bodies. Infrastructure
classifications deliberately omit underlying provider/store text except for
context cancellation/deadline identity. The compiler still stages only and the
existing pre-stage retraction watermark check is untouched.

Remaining concern: this sandbox prevents the native Gemini fake-server test
from binding loopback. The deterministic parser tests and all Brain/config
tests passed locally; run the native `httptest` test in a normal CI/developer
environment for final transport-wire confirmation.
