package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// GameSDKHooksConfig holds configurable domain rules for the Game SDK assistant
// extension so generic engine files never hardcode KB names, device mappings,
// A2UI button rules, or Chinese refusal heuristics.
type GameSDKHooksConfig struct {
	// DeviceToKBNames maps lowercase device tokens (e.g. "手游", "端游", "pc")
	// to knowledge base names that should be included within authorized scope.
	DeviceToKBNames map[string][]string
	// MaxFollowUpButtons caps standalone <button-send> items on normal answers.
	MaxFollowUpButtons int
	// WikiPlaceholderWarnings prevents fast-answer bypass when wiki content contains any marker.
	WikiPlaceholderWarnings []string
}

// DefaultGameSDKHooksConfig returns the production Game SDK ruleset.
func DefaultGameSDKHooksConfig() GameSDKHooksConfig {
	return GameSDKHooksConfig{
		DeviceToKBNames: map[string][]string{
			"手游":   {"docs-client"},
			"端游":   {"docs-pc"},
			"pc":   {"docs-pc"},
			"H5端":  {"docs-h5"},
			"Web端": {"docs-h5"},
		},
		MaxFollowUpButtons:      3,
		WikiPlaceholderWarnings: []string{"占位", "未终审", "未审核", "未经审核", "待审核", "待确认"},
	}
}

// GameSDKHooks implements EngineHooks for the Game SDK vertical extension.
type GameSDKHooks struct {
	cfg GameSDKHooksConfig
}

// NewGameSDKHooks creates a stateless GameSDKHooks instance.
func NewGameSDKHooks(cfg GameSDKHooksConfig) *GameSDKHooks {
	if cfg.MaxFollowUpButtons <= 0 {
		cfg.MaxFollowUpButtons = 3
	}
	return &GameSDKHooks{cfg: cfg}
}

var defaultEngineHooks EngineHooks = NewGameSDKHooks(DefaultGameSDKHooksConfig())

const maxFollowUpButtons = 3

var (
	buttonSend = regexp.MustCompile(`(?m)^[ \t]*<button-send>([^<>]+)</button-send>[ \t]*\n?`)
	// Match standalone button lines (single or multiple buttons on one line),
	// optionally prefixed with list numbers or bullets.
	buttonSendLine      = regexp.MustCompile(`^[ \t]*(?:(?:\d+[.)、]|[-*+•])[ \t]+)?<button-send>([^<>]+)</button-send>[ \t]*$`)
	buttonSendTag       = regexp.MustCompile(`<button-send>([^<>]*)</button-send>`)
	buttonSendLineMulti = regexp.MustCompile(`^[ \t]*(?:(?:\d+[.)、]|[-*+•])[ \t]+)?(?:<button-send>[^<>]*</button-send>[ \t,;，；、|/]*)+$`)
	inlineButtonSend    = regexp.MustCompile(`<button-send>.*?</button-send>`)
	followUpHeadingLine = regexp.MustCompile(`(?i)^[ \t]*(?:#{1,6}[ \t]+)?(?:\*{1,2})?[ \t]*(?:相关问题|猜你想问|你可能还想问|您可能还想问|推荐问题|延伸问题|继续提问|下一步(?:建议|可问|问题)?|related questions|follow-up questions|suggested questions)[ \t：:]*(?:\*{1,2})?[ \t：:]*$`)
	noAnswerOrRefusal   = regexp.MustCompile(`(?:无法回答与[^。！？\n]{0,24}无关|超出(?:服务|解答|支持)范围|(?:当前)?知识库(?:中)?(?:没有|未)(?:查询到|找到|检索到)[^。！？\n]{0,30}(?:解答|答案|对应|相关))`)
	// "X 不支持 Y", "X：不支持", "| X | 不支持 |" — X and Y stop at clause/cell boundaries.
	unsupportedClaim = regexp.MustCompile(`([^，,。；;！!？?\n|：:]*?)[\s|：:]*(?:暂|目前|均|都|并|尚|还|也)*不支持([^，,。；;！!？?\n|]*)`)
)

func renderGenericAppInfoBlock(appInfo string) string {
	appInfo = types.SanitizeLiveAppInfoJSON(appInfo)
	return fmt.Sprintf("  <app_info type=\"static_project_metadata\" live_config_verified=\"false\">%s</app_info>\n", escapeXMLText(appInfo))
}

// RenderAppInfo wraps raw app_info in a model-facing <app_info> envelope
// annotated as static project metadata and normalizes multi-value device_type
// strings without mutating e.appInfo.
func (h *GameSDKHooks) RenderAppInfo(appInfo string) string {
	return renderAppInfoBlock(appInfo)
}

func renderAppInfoBlock(appInfo string) string {
	appInfo = types.SanitizeLiveAppInfoJSON(appInfo)
	attrs := ` type="static_project_metadata" live_config_verified="false"`
	note := ""
	if devices := parseAppInfoDeviceTypes(appInfo); len(devices) > 1 {
		attrs += fmt.Sprintf(` multi_device_type="true" device_type_values="%s"`, escapeXMLAttr(strings.Join(devices, ",")))
		note = "<!-- Note: device_type lists multiple platforms (see device_type_values attribute); do not assume a single target platform from app_info alone. -->"
	}
	if note != "" {
		return fmt.Sprintf("  <app_info%s>%s\n  %s</app_info>\n", attrs, escapeXMLText(appInfo), note)
	}
	return fmt.Sprintf("  <app_info%s>%s</app_info>\n", attrs, escapeXMLText(appInfo))
}

func parseAppInfoDeviceTypes(appInfo string) []string {
	var raw map[string]any
	if err := json.Unmarshal([]byte(appInfo), &raw); err != nil || raw == nil {
		return nil
	}
	var deviceStr string
	if gi, ok := raw["gameInfo"].(map[string]any); ok {
		deviceStr, _ = gi["device_type"].(string)
	}
	if strings.TrimSpace(deviceStr) == "" {
		deviceStr, _ = raw["device_type"].(string)
	}
	if strings.TrimSpace(deviceStr) == "" {
		return nil
	}
	parts := strings.FieldsFunc(deviceStr, func(r rune) bool {
		switch r {
		case '、', ',', '，', '/', '|', ';', '；', ' ', '\t':
			return true
		default:
			return false
		}
	})
	var out []string
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// OnBeforeToolDispatch rewrites one document search when FAQ preflight prepared
// a broader document query.
func (h *GameSDKHooks) OnBeforeToolDispatch(e *AgentEngine, response *types.ChatResponse) {
	e.useFAQBroadDocumentSearch(response)
}

// useFAQBroadDocumentSearch replaces one model-selected document search while
// preserving the caller's resolved retrieval targets and the other tool calls.
func (e *AgentEngine) useFAQBroadDocumentSearch(response *types.ChatResponse) {
	if e.faqBroadDocumentQuery == "" || response == nil || isLengthFinishReason(response.FinishReason) {
		return
	}
	if slices.ContainsFunc(response.ToolCalls, func(call types.LLMToolCall) bool {
		return call.Function.Name == agenttools.ToolAskUser || call.Function.Name == agenttools.ToolWikiSearch
	}) {
		return
	}
	for i := range response.ToolCalls {
		call := &response.ToolCalls[i]
		if call.Function.Name != agenttools.ToolSearchKnowledge || len(call.UnresolvedHandles) != 0 {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil || args["retrieval"] != nil {
			continue
		}
		ids, idsOK := args["knowledge_base_ids"].([]any)
		if args["knowledge_base_ids"] != nil && !idsOK {
			continue
		}
		validIDs := true
		for _, id := range ids {
			name, ok := id.(string)
			if !ok || e.config == nil || !slices.Contains(e.config.SearchTargets.GetAllKnowledgeBaseIDs(), name) {
				validIDs = false
				break
			}
		}
		if !validIDs {
			continue
		}
		document := false
		for _, kb := range e.knowledgeBasesInfo {
			if kb == nil || kb.Type != "document" || !slices.Contains(kb.Capabilities, "chunks") {
				continue
			}
			if len(ids) == 0 || slices.Contains(ids, any(kb.ID)) {
				document = true
				break
			}
		}
		if !document {
			continue
		}
		args["query"] = e.faqBroadDocumentQuery
		delete(args, "knowledge_base_ids")
		delete(args, "metadata")
		delete(args, "mode")
		encoded, err := json.Marshal(args)
		if err != nil {
			return
		}
		if call.ModelArguments == "" {
			call.ModelArguments = call.Function.Arguments
		}
		call.Function.Arguments = string(encoded)
		e.faqBroadDocumentQuery = ""
		return
	}
}

// ExpandSearchKnowledgeBases appends configured platform integration KBs (e.g.
// docs-client / docs-pc) only within the already authorized SearchTargets scope.
func (h *GameSDKHooks) ExpandSearchKnowledgeBases(e *AgentEngine, args map[string]any) {
	ids, ok := args["knowledge_base_ids"].([]any)
	if !ok || len(ids) == 0 || e.retrieval == nil || e.config == nil || len(h.cfg.DeviceToKBNames) == 0 {
		return
	}
	if hint, ok := args["retrieval"].(map[string]any); ok && (hint["action"] == "recovery" || hint["scope_from"] != nil) {
		return
	}
	state := e.retrieval.snapshot()
	original, err := json.Marshal(retrievalToolArgs(args))
	if err != nil {
		return
	}
	for i := range state.Attempts {
		attempt := &state.Attempts[i]
		if attempt.QuestionID == state.questionID() && len(attempt.Failures) > 0 && validateScopedRecovery(attempt, string(original)) == nil {
			return
		}
	}
	hasSearch := false
	for i := len(state.Attempts) - 1; i >= 0; i-- {
		attempt := state.Attempts[i]
		if attempt.QuestionID == state.questionID() && isBudgetedSearchTool(attempt.Tool) {
			hasSearch = attempt.Completed && attempt.Outcome == retrievalSuccess && len(attempt.Failures) == 0
			break
		}
	}
	if !hasSearch {
		return
	}
	devices := parseGameInfoDeviceTypes(e.appInfo)
	if len(devices) == 0 {
		return
	}
	names := map[string]bool{}
	for _, device := range devices {
		for _, kbName := range h.cfg.DeviceToKBNames[strings.ToLower(strings.TrimSpace(device))] {
			names[kbName] = true
		}
	}
	if len(names) == 0 {
		return
	}
	for _, id := range ids {
		value, ok := id.(string)
		if !ok || !slices.ContainsFunc(e.config.SearchTargets, func(target *types.SearchTarget) bool {
			return target != nil && target.KnowledgeBaseID == value
		}) {
			return
		}
	}
	if e.allFAQKnowledgeBases(ids) {
		return
	}
	for _, kb := range e.knowledgeBasesInfo {
		if kb == nil || !names[kb.Name] || !slices.ContainsFunc(e.config.SearchTargets, func(target *types.SearchTarget) bool {
			return target != nil && target.KnowledgeBaseID == kb.ID
		}) || slices.Contains(ids, any(kb.ID)) {
			continue
		}
		ids = append(ids, kb.ID)
	}
	args["knowledge_base_ids"] = ids
}

// parseGameInfoDeviceTypes strictly parses gameInfo.device_type using the
// original comma/ideographic-comma/semicolon delimiters for KB expansion,
// keeping expansion scope strictly equivalent to the pre-refactor logic.
func parseGameInfoDeviceTypes(appInfo string) []string {
	var app struct {
		GameInfo struct {
			DeviceType string `json:"device_type"`
		} `json:"gameInfo"`
	}
	if json.Unmarshal([]byte(appInfo), &app) != nil {
		return nil
	}
	return strings.FieldsFunc(app.GameInfo.DeviceType, func(r rune) bool {
		return r == '、' || r == ',' || r == '，' || r == ';' || r == '；'
	})
}

// includeProjectIntegrationLibraries delegates to the active EngineHooks so
// existing unit tests keep working while the engine stays free of KB name hardcoding.
func (e *AgentEngine) includeProjectIntegrationLibraries(args map[string]any) {
	e.extHooks().ExpandSearchKnowledgeBases(e, args)
}

func (e *AgentEngine) allFAQKnowledgeBases(ids []any) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		value, ok := id.(string)
		if !ok || !slices.ContainsFunc(e.knowledgeBasesInfo, func(kb *KnowledgeBaseInfo) bool {
			return kb != nil && kb.ID == value && kb.Type == types.KnowledgeBaseTypeFAQ
		}) {
			return false
		}
	}
	return true
}

func (h *GameSDKHooks) IsResourceNavigationFastAnswer(e *AgentEngine, messages []chat.Message) bool {
	if e.config == nil || !e.config.ResourceNavigationFastAnswer || e.retrieval == nil || len(e.pinnedSkills) > 0 || len(e.pinnedMCPServices) > 0 {
		return false
	}
	calls, users := 0, 0
	for _, message := range messages {
		if message.Role == "user" {
			users++
		}
		for _, call := range message.ToolCalls {
			if call.Function.Name != agenttools.ToolWikiReadPage {
				return false
			}
			calls++
		}
	}
	if calls != 1 || users != 1 {
		return false
	}
	state := e.retrieval.snapshot()
	var wiki *retrievalAttempt
	for i := range state.Attempts {
		a := &state.Attempts[i]
		if wiki != nil || a.QuestionID != state.questionID() || a.Tool != agenttools.ToolWikiReadPage || !a.Completed || a.Outcome != retrievalSuccess || len(a.Failures) > 0 {
			return false
		}
		wiki = a
	}
	if wiki == nil || strings.Count(wiki.Content, "<wiki_page>") != 1 || strings.Contains(wiki.Content, "<omitted_pages") || strings.Contains(wiki.Content, "[output truncated:") || strings.Contains(wiki.Content, "(body omitted:") {
		return false
	}
	for _, warning := range h.cfg.WikiPlaceholderWarnings {
		if strings.Contains(wiki.Content, warning) {
			return false
		}
	}
	_, body, ok := strings.Cut(wiki.Content, "<content>")
	if !ok {
		return false
	}
	body, _, ok = strings.Cut(body, "</content>")
	return ok && strings.TrimSpace(body) != ""
}

func (e *AgentEngine) resourceNavigationHasOneWiki(messages []chat.Message) bool {
	return e.extHooks().IsResourceNavigationFastAnswer(e, messages)
}

func (h *GameSDKHooks) PostProcessAnswer(e *AgentEngine, answer string) string {
	return e.sanitizeFollowUpButtonsWithCap(answer, h.cfg.MaxFollowUpButtons)
}

func (h *GameSDKHooks) StripFollowUpButtons(answer string) string {
	return stripFollowUpButtons(answer)
}

func (e *AgentEngine) isNoAnswerOrRefusalBody(body string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || e.isRuntimeFallbackAnswer(trimmed) || e.isFixedResponseValue(trimmed) {
		return true
	}
	candidate := strings.TrimSpace(stripTrailingReferenceSection(unwrapOuterMarkdownFence(trimmed)))
	if candidate == "" || e.isRuntimeFallbackAnswer(candidate) || e.isFixedResponseValue(candidate) {
		return true
	}
	semanticBody := strings.TrimSpace(unfencedText(stripTrailingReferenceSection(trimmed)))
	if len([]rune(semanticBody)) <= 220 && !strings.HasPrefix(semanticBody, "## ") && !strings.Contains(semanticBody, "\n## ") &&
		noAnswerOrRefusal.MatchString(semanticBody) {
		return true
	}
	return false
}

// removePrecedingFollowUpHeading clears a standalone "相关问题" heading directly
// above a removed or normalized button block (skipping blank lines).
func removePrecedingFollowUpHeading(lines []string, removed []bool, from int) {
	for i := from - 1; i >= 0; i-- {
		if removed[i] || strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if followUpHeadingLine.MatchString(lines[i]) {
			removed[i] = true
		}
		return
	}
}

// stripFollowUpButtons removes all <button-send> tags and any standalone
// follow-up heading immediately preceding them.
func stripFollowUpButtons(answer string) string {
	if !strings.Contains(answer, "<button-send>") {
		return answer
	}
	lines := strings.Split(answer, "\n")
	removed := make([]bool, len(lines))
	protected := fencedLines(lines)
	for i, line := range lines {
		if !protected[i] && (buttonSendLineMulti.MatchString(line) || buttonSendLine.MatchString(line)) {
			removed[i] = true
			removePrecedingFollowUpHeading(lines, removed, i)
		}
	}
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if removed[i] {
			continue
		}
		if !protected[i] && strings.Contains(line, "<button-send>") {
			cleaned := strings.TrimSpace(inlineButtonSend.ReplaceAllString(line, ""))
			if cleaned == "" {
				continue
			}
			line = cleaned
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), " \t\r\n")
}

// sanitizeFollowUpButtons enforces deterministic <button-send> rules at answer
// publication: strip all buttons on fallback/refusal/clarification turns, and
// normalize, split multi-button lines, deduplicate, drop contradicted items,
// and cap to maxButtons on normal turns.
func (e *AgentEngine) sanitizeFollowUpButtons(answer string) string {
	return e.sanitizeFollowUpButtonsWithCap(answer, maxFollowUpButtons)
}

func (e *AgentEngine) sanitizeFollowUpButtonsWithCap(answer string, maxButtons int) string {
	if !strings.Contains(answer, "<button-send>") {
		return answer
	}
	if maxButtons <= 0 {
		maxButtons = maxFollowUpButtons
	}
	body := stripFollowUpButtons(answer)
	if e.hasMissingUserFields() || e.isNoAnswerOrRefusalBody(body) {
		return body
	}

	rawLines := strings.Split(answer, "\n")
	lines := make([]string, 0, len(rawLines))
	hadDroppedEmptyButton := false
	rawProtected := fencedLines(rawLines)
	for i, line := range rawLines {
		if !rawProtected[i] && buttonSendLineMulti.MatchString(line) {
			matches := buttonSendTag.FindAllStringSubmatch(line, -1)
			emittedAny := false
			for _, m := range matches {
				text := strings.TrimSpace(m[1])
				if text == "" {
					hadDroppedEmptyButton = true
					continue
				}
				lines = append(lines, "<button-send>"+text+"</button-send>")
				emittedAny = true
			}
			if !emittedAny && hadDroppedEmptyButton {
				for j := len(lines) - 1; j >= 0; j-- {
					if strings.TrimSpace(lines[j]) == "" {
						continue
					}
					if followUpHeadingLine.MatchString(lines[j]) {
						lines = append(lines[:j], lines[j+1:]...)
					}
					break
				}
			}
			continue
		}
		lines = append(lines, line)
	}

	removed := make([]bool, len(lines))
	seen := make(map[string]bool)
	protected := fencedLines(lines)
	for i, line := range lines {
		if protected[i] {
			continue
		}
		match := buttonSendLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		removePrecedingFollowUpHeading(lines, removed, i)
		text := strings.TrimSpace(match[1])
		if text == "" || seen[text] {
			removed[i] = true
			continue
		}
		seen[text] = true
		lines[i] = "<button-send>" + text + "</button-send>"
	}
	normalized := make([]string, 0, len(lines))
	for i, line := range lines {
		if !removed[i] {
			normalized = append(normalized, line)
		}
	}

	filtered := dropContradictedButtons(strings.Join(normalized, "\n"))

	filteredLines := strings.Split(filtered, "\n")
	capped := make([]string, 0, len(filteredLines))
	keptButtons := 0
	filteredProtected := fencedLines(filteredLines)
	for i, line := range filteredLines {
		if !filteredProtected[i] && buttonSendLine.MatchString(line) {
			if keptButtons >= maxButtons {
				continue
			}
			keptButtons++
		}
		capped = append(capped, line)
	}
	return strings.TrimRight(strings.Join(capped, "\n"), " \t\r\n")
}

// claimPart trims list/table/quote markers; deictic subjects ("该SDK") and
// one-rune leftovers carry no matchable name.
func claimPart(s string, subject bool) string {
	s = strings.Trim(s, " \t-*+#>")
	if subject {
		for _, p := range []string{"该", "本", "此", "这", "当前"} {
			if strings.HasPrefix(s, p) {
				return ""
			}
		}
	}
	if len([]rune(s)) < 2 {
		return ""
	}
	return s
}

// dropContradictedButtons removes follow-up buttons that name what the body
// itself declared unsupported. The body text is never modified.
func dropContradictedButtons(answer string) string {
	body := buttonSend.ReplaceAllString(unfencedText(answer), "")
	var claims [][2]string
	for _, m := range unsupportedClaim.FindAllStringSubmatch(body, -1) {
		if strings.HasSuffix(m[1], "非") || strings.HasSuffix(m[1], "否") { // 并非不支持 / 是否不支持
			continue
		}
		if c := [2]string{claimPart(m[1], true), claimPart(m[2], false)}; c[0] != "" || c[1] != "" {
			claims = append(claims, c)
		}
	}
	if len(claims) == 0 {
		return answer
	}
	lines := strings.SplitAfter(answer, "\n")
	protected := fencedLines(lines)
	for i, line := range lines {
		if protected[i] {
			continue
		}
		lines[i] = buttonSend.ReplaceAllStringFunc(line, func(line string) string {
			text := buttonSend.FindStringSubmatch(line)[1]
			for _, c := range claims {
				if (c[0] == "" || strings.Contains(text, c[0])) && (c[1] == "" || strings.Contains(text, c[1])) {
					return ""
				}
			}
			return line
		})
	}
	return strings.Join(lines, "")
}
