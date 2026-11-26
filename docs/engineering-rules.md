# WeKnora Engineering Rules

Version-controlled engineering invariants distilled from production postmortems and rework reviews.

## 1. Source Admission Invariant (Structured Sources Over Display Text)
- **Single source of truth**: Every KB/Wiki tool call explicitly publishes its readable document/chunk IDs via structured `ToolResult.PublishedSources` / `ToolResult.Data`, and `retrievalAttempt` persists `StructuredSources`, `ObservedIDs`, and `CitableIDs`.
- **Navigable candidates vs. citable evidence**: Record navigable-only references (`omitted_candidates`, `list_documents`, `wiki_read_page` `<sources>`) in `ObservedIDs` only; record retrieved evidence (`search_knowledge` hits, `read_document` chunks) in both `ObservedIDs` and `CitableIDs`.
- **No body-text spoofing**: When `StructuredSources == true`, control-plane admission must NEVER fall back to scanning model-facing XML/Markdown output text. Handle registries (`modelcontext`) resolve identity; per-call `ToolResult` declares what was actually published.
- **Quote verification stays separate**: Structured source IDs establish identity and provenance; verifying that a claim is supported by source text/quotes remains an independent check.
- Trace a retrieval change through its production entry, scope, ranking, output budget and resume paths. Published references and the source actually shown to the model must remain aligned.
- Check related callers and input variants for the same failure when fixing a boundary.

## 2. FAQ Scope & Reflow Invariants
- **FAQ scope isolation**: Automatic library expansion (e.g. `includeProjectIntegrationLibraries`) must never alter an explicit FAQ-only retrieval scope, while explicit hybrid searches from the user or agent keep their selected scope.
- **FAQ reflow preserves rejection constraints**: Partial-coverage candidates (`miss_insufficient`) remain available for follow-up exact lookup (`FAQPreflightQuestions`) without injecting unreviewed titles into `<faq_preflight_candidates>`; candidates rejected with an explicit reason (`needs_live_data`, `miss_review_rejected`) retain their rejection reason (`FAQPreflightRejections`) so both prompt hints and follow-up lookups surface the constraint instead of dropping it.
- Bind entity-specific decisions across stages to the existing stable entity ID and scope. Use titles as search hints, and test distinct entities with the same title.
- Bind asynchronous work to the question and turn that started it. Test that an older result cannot affect a later turn or steer.

## 3. Cross-Tool Contract Matrix
- Any change to `internal/agent/retrieval_*.go`, `internal/modelcontext/`, or active KB/Wiki tools must keep `TestKBToolToReadDocumentContractMatrix` green across **both** runtime and structured retrieval paths, covering:
  - valid follow-up `read_document`,
  - body-text tags ignored,
  - navigable candidates excluded from `CitableIDs`,
  - scope narrowing/change revoking out-of-scope observations,
  - suspend/resume JSON snapshot round-trip.

## 4. Structured LLM Reviewer Truncation & Pre-Commit Gate
- Apply the same completion-token truncation semantics to structured JSON reviewers. The agent layer uses `isLengthFinishReason` and the service layer uses `isLengthStopFinishReason`.
- Run `make pre-commit` before committing backend/API changes. It checks Swagger, agent and modelcontext packages, plus selected FAQ and AgentQA service tests. Run `make docs` when Swagger-documented APIs or agent config schemas change.
- When changing provider request serialization or defaults, run `make test-model-contract` and review and update affected request baselines in the same change. App CI also runs these tests. Provider hostname resolution to fake IPs can cause the SSRF check to fail locally; record that failure and use an environment with normal DNS for verification.
- A skipped check is not a pass. `make pre-commit` reports when `cmd/nocomment` is missing. A clean checkout must contain every in-repository script and source referenced by a changed workflow, and the same change must verify those workflow commands.
- Record a rollback's object, environment and verified outcome. Git author dates alone do not establish deployment or rollback timing.

## 5. AgentQA Replay and Evaluation
- Before comparing AgentQA behavior, identify the actual request entry point and record the running code version, Agent configuration, knowledge scope, and loaded Prompt and Skill versions. Use counterexamples and traces to check what evidence reached the model and what the user received.
- When evaluating a judge change, re-score the same preserved answers and compare the judgments separately from changes to the answering system.
