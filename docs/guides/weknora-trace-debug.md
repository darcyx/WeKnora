# WeKnora Trace Debugging Guide

Standard workflow for root-causing WeKnora Agent UI errors (e.g. "调用 查看文档 失败", unexpected fallback, or missing tool calls) using Langfuse traces.

## 1. Map UI Error Text to Tool Names
Check `frontend/src/views/chat/components/AgentStreamDisplay.vue` and `frontend/src/i18n/locales/zh-CN/agent.ts` to map localized tool labels to backend tool names:
- `查看文档` → `read_document` / `get_document_content` / `wiki_read_source_doc`
- `检索知识库` → `search_knowledge` / `knowledge_search`
- `获取文档信息` → `get_document_info` / `list_documents`

## 2. Fetch Session Traces via Langfuse API
Load `LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY` and query:
- List traces: `GET {LANGFUSE_HOST}/api/public/traces?sessionId={session_id}&limit=50`
- Expand observations: `GET {LANGFUSE_HOST}/api/public/traces/{trace_id}?expand=observations`

## 3. Distinguish Pre-Dispatch Rejection, Post-Span Rejection, and Tracing Disabled
Do not rely on missing spans alone:
- **Pre-dispatch rejection** before `StartSpan` can leave no tool span. Inspect the emitting `GENERATION.output.tool_calls` and the next round's `role == "tool"` / `role == "system"` messages for the actual failure.
- **Post-span rejection / tool error** (`act.go` after `StartSpan`, including unresolved handles, tool-internal validation, or backend error): an `agent.tool.<name>` span can record `output.success == false` or `output.error`.
- **Tracing disabled**: if Langfuse is disabled for the instance/request, no spans exist at all; verify via application logs and stored session messages (`agent_steps`).

## 4. Map to Code & Lock In With Contract Tests
- `Retrieval not dispatched: ...` → `internal/agent/act.go` → `retrieval_dispatch.go` → `retrieval_runtime.go` → `retrieval_guard.go`.
- Published source mismatch → `ToolResult.PublishedSources` / `extractToolPublishedSources` (`internal/agent/retrieval_guard.go`) and `internal/modelcontext/`.
- FAQ preflight mismatch / truncation → `internal/application/service/session_agent_faq_clarification.go` and `faq_review` `finish_reason` (`isLengthStopFinishReason`).
- Add a regression case to `TestKBToolToReadDocumentContractMatrix` (`internal/agent/retrieval_read_admission_test.go`) and run `make pre-commit`.
