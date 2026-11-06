package personal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
	"kagari/internal/textpage"
)

// Opt-in comparison of real summary and answer calls, using only fictional history.
func TestLivePersonalContextCompressionComparison(t *testing.T) {
	// Fix the experiment's admission budget without claiming the endpoint's advertised capacity.
	t.Setenv("KAGARI_MODEL_CONTEXT_WINDOW", "32768")
	engine := livePersonalEngine(t)
	window := engine.Config.Model.ContextWindow
	output := int64(engine.Config.Model.MaxOutputTokens)
	limits, err := inputbudget.NewLimits(window, output)
	if err != nil {
		t.Fatalf("configured context capacity cannot support the local evaluation budget (window=%d output=%d)", window, output)
	}
	t.Logf("local evaluation budget (not endpoint capacity): test_window=%d output=%d summarize_at=%d hard=%d count_basis=%s", limits.Window, limits.Output, limits.SummarizeAt, limits.Hard, inputbudget.CountBasis)

	logs, observed := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithTimeout(logging.WithLogger(context.Background(), zap.New(logs)), 3*time.Minute)
	defer cancel()
	store := testStore(t, filepath.Join(t.TempDir(), "context-compression.db"), 15*time.Minute)
	seq := int64(7000)
	run := acceptanceRunningTask(t, store, &seq, "context-compression-live", TaskInput{Goal: "Compare synthetic context handling"})
	scope := ReadScope{RunID: run.ID}
	code := liveInputCode(t)
	materialBytes := int(min(int64(4000), max(int64(100), (limits.Hard-limits.SummarizeAt)/3)))
	materialBody := liveCompressionSource(code, materialBytes)
	ref, err := store.SaveSource(ctx, run.ID, "fictional-context-source", domain.Source{
		URL: "https://example.org/fictional-context-record", Title: "Fictional context record", Status: "ok",
		FetchedAt: time.Now().UTC(), Content: materialBody,
	})
	if err != nil {
		t.Fatal("cannot save fictional source material")
	}
	beforeBody, err := store.ReadMaterial(ctx, scope, ref.ID, "body", textpage.Request{Limit: textpage.MaxTextLimit})
	if err != nil {
		t.Fatal("cannot read saved fictional source material")
	}
	beforeMetadata, err := store.ReadMaterial(ctx, scope, ref.ID, "metadata", textpage.Request{})
	if err != nil {
		t.Fatal("cannot read saved fictional source metadata")
	}
	history, estimated, reducedEstimate, err := liveCompressionHistory(ctx, store, scope, ref.ID, code, limits)
	if err != nil {
		t.Fatalf("cannot construct budgeted fictional history: %v", err)
	}
	if estimated.TotalBytes <= limits.SummarizeAt || estimated.TotalBytes >= limits.Hard || reducedEstimate.TotalBytes <= limits.SummarizeAt {
		t.Fatalf("fixture estimates do not bracket the summary trigger: original=%d reduced=%d summarize_at=%d hard=%d", estimated.TotalBytes, reducedEstimate.TotalBytes, limits.SummarizeAt, limits.Hard)
	}
	t.Logf("fictional history estimate: original=%d after_early_tool_result_reduction=%d summarize_at=%d hard=%d", estimated.TotalBytes, reducedEstimate.TotalBytes, limits.SummarizeAt, limits.Hard)

	var mu sync.Mutex
	var modelRecords []inputbudget.ModelRecord
	bounded, err := inputbudget.NewAgenticModel(engine.Model, inputbudget.ModelConfig{
		Limits: limits,
		Observe: func(record inputbudget.ModelRecord) {
			mu.Lock()
			modelRecords = append(modelRecords, record)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal("cannot configure test-only input budget")
	}

	baselineMessages := cloneLiveCompressionMessages(t, history)
	started := time.Now()
	baselineReply, err := bounded.Generate(ctx, baselineMessages)
	baselineDuration := time.Since(started).Round(time.Millisecond)
	if err != nil {
		t.Fatal("baseline real-model request failed (endpoint details withheld)")
	}
	if hasLiveCompressionToolCall(baselineReply) {
		t.Fatal("baseline response attempted a tool call; this comparison uses direct Generate only")
	}
	if !liveCompressionAnswerCorrect(baselineReply, code, ref.ID) {
		t.Fatal("baseline response omitted the fictional decision, verification code, or source ID")
	}
	mu.Lock()
	baselineRecords := append([]inputbudget.ModelRecord(nil), modelRecords...)
	mu.Unlock()
	if len(baselineRecords) != 1 {
		t.Fatalf("baseline model call count=%d, want 1", len(baselineRecords))
	}

	traceStore := &liveCompressionReadTraceStore{Store: store}
	compressedMessages := cloneLiveCompressionMessages(t, history)
	handler, err := contextBudgetHandler(ctx, bounded, traceStore, scope)
	if err != nil {
		t.Fatal("cannot configure production context budget handler")
	}
	compressedCtx, _, err := handler.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal("production context handler setup failed")
	}
	started = time.Now()
	compressedCtx, rewritten, err := handler.BeforeModelRewriteState(compressedCtx, &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: compressedMessages}, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil {
		t.Fatal("production context rewrite or real summary call failed (endpoint details withheld)")
	}
	if rewritten == nil || len(rewritten.Messages) == 0 {
		t.Fatal("production context rewrite returned no messages")
	}
	compressedReply, err := bounded.Generate(compressedCtx, cloneLiveCompressionMessages(t, rewritten.Messages))
	compressedDuration := time.Since(started).Round(time.Millisecond)
	if err != nil {
		t.Fatal("compressed real-model request failed (endpoint details withheld)")
	}
	if hasLiveCompressionToolCall(compressedReply) {
		t.Fatal("compressed response attempted a tool call; this comparison uses direct Generate only")
	}
	var summaryCalls, summaryApplied int
	for _, entry := range observed.FilterMessage("context.reduced").All() {
		phase, _ := entry.ContextMap()["phase"].(string)
		switch phase {
		case "summary_model":
			summaryCalls++
		case "summary_applied":
			summaryApplied++
		}
	}
	if summaryApplied < 1 {
		t.Fatal("production context handler did not apply a real model summary")
	}
	mu.Lock()
	allRecords := append([]inputbudget.ModelRecord(nil), modelRecords...)
	mu.Unlock()
	if len(allRecords) != 3 {
		t.Fatalf("real model Generate count=%d, want exactly baseline 1 + summary 1 + compressed final 1 (maximum 3)", len(allRecords))
	}
	if summaryCalls < 1 {
		t.Fatal("production handler did not record its real summary-model call")
	}

	afterBody, err := store.ReadMaterial(ctx, scope, ref.ID, "body", textpage.Request{Limit: textpage.MaxTextLimit})
	if err != nil {
		t.Fatal("saved source became unreadable after context compression")
	}
	afterMetadata, err := store.ReadMaterial(ctx, scope, ref.ID, "metadata", textpage.Request{})
	if err != nil {
		t.Fatal("saved source metadata became unreadable after context compression")
	}
	if beforeBody.Page.Text != afterBody.Page.Text || beforeBody.Page.Version != afterBody.Page.Version || beforeBody.Material.Version != afterBody.Material.Version ||
		beforeMetadata.Page.Text != afterMetadata.Page.Text || beforeMetadata.Page.Version != afterMetadata.Page.Version {
		t.Fatal("context compression changed saved source content or collection version")
	}
	materials, err := store.ListMaterials(ctx, scope, run.ID, ListPage{})
	if err != nil || len(materials.Items) != 1 || materials.Items[0].ID != ref.ID {
		t.Fatal("context compression archived an already readable source reference again")
	}

	compressedRecords := allRecords[1:]
	finalRecord := compressedRecords[len(compressedRecords)-1]
	var compressedInputBytes, compressedInputTokens, compressedOutputTokens int64
	for _, record := range compressedRecords {
		compressedInputBytes += record.Estimate.TotalBytes
		compressedInputTokens += int64(record.InputTokens)
		compressedOutputTokens += int64(record.OutputTokens)
	}
	t.Logf("baseline: calls=%d input_utf8_bytes=%d input_tokens=%d output_tokens=%d duration=%s model_material_tool_calls=0", len(baselineRecords), baselineRecords[0].Estimate.TotalBytes, baselineRecords[0].InputTokens, baselineRecords[0].OutputTokens, baselineDuration)
	t.Logf("compressed final request: input_utf8_bytes=%d input_tokens=%d output_tokens=%d", finalRecord.Estimate.TotalBytes, finalRecord.InputTokens, finalRecord.OutputTokens)
	t.Logf("compressed path total: calls=%d summary_calls=%d summary_applied=%d input_utf8_bytes=%d input_tokens=%d output_tokens=%d duration=%s model_material_tool_calls=0 summary_validation_material_reads=%d explicit_verification_reads=2", len(compressedRecords), summaryCalls, summaryApplied, compressedInputBytes, compressedInputTokens, compressedOutputTokens, compressedDuration, traceStore.reads)
	if !hasUsage(baselineRecords[0]) || !hasUsage(compressedRecords[0]) || !hasUsage(finalRecord) {
		t.Log("provider token usage was absent for at least one real model call; UTF-8 estimates and call counts remain available")
	}
	if !liveCompressionAnswerCorrect(compressedReply, code, ref.ID) {
		t.Fatal("compressed response omitted the fictional decision, verification code, or source ID")
	}
}

func liveCompressionSource(code string, targetBytes int) string {
	base := "Fictional case ARGO. Decision: APPROVE. Verification code: " + code + "."
	if len(base) < targetBytes {
		base += strings.Repeat("x", targetBytes-len(base))
	}
	return base
}

func liveCompressionHistory(ctx context.Context, store *Store, scope ReadScope, sourceID, code string, limits inputbudget.Limits) ([]*schema.AgenticMessage, inputbudget.Estimate, inputbudget.Estimate, error) {
	body, err := store.ReadMaterial(ctx, scope, sourceID, "body", textpage.Request{Limit: textpage.MaxTextLimit})
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	metadata, err := store.ReadMaterial(ctx, scope, sourceID, "metadata", textpage.Request{})
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	const earlyCallID = "fictional_early_read"
	const recentCallID = "fictional_recent_read"
	facts := "Fictional archive note: case ARGO decision=APPROVE; verification_code=" + code + "; source_id=" + sourceID + ". These values are synthetic test evidence."
	build := func(fillerChars int) []*schema.AgenticMessage {
		filler := strings.Repeat("x", fillerChars)
		third := len(filler) / 3
		messages := []*schema.AgenticMessage{
			schema.SystemAgenticMessage("You review fictional records. Use only the supplied history and keep exact evidence identifiers, decisions, and verification codes."),
			schema.UserAgenticMessage("这是一个虚构档案评估。只整理历史记录，不要联网。"),
			liveCompressionToolCall(earlyCallID, sourceID, "body"),
			liveCompressionToolResult(earlyCallID, "read_material", string(bodyJSON)),
			liveCompressionAssistantMessage(facts + "\nHistorical note 1: " + filler[:third]),
			schema.UserAgenticMessage("虚构历史笔记 2：" + filler[third:2*third]),
			liveCompressionAssistantMessage("Historical note 3: " + filler[2*third:]),
			liveCompressionToolCall(recentCallID, sourceID, "metadata"),
			liveCompressionToolResult(recentCallID, "read_material", string(metadataJSON)),
			schema.UserAgenticMessage("根据此前虚构记录，严格返回单行 decision=<值>; verification_code=<值>; source_id=<值>。decision 只能是 APPROVE 或 REJECT；approved 等批准措辞统一输出 APPROVE。校验码和来源 ID 必须逐字保留。不要调用工具或解释。"),
		}
		return messages
	}
	first := build(0)
	round := inputbudget.ToolRound{Key: "fictional_early_material_read", Start: 2, End: 4, Complete: true, ToolNames: []string{"read_material"}, Messages: first[2:4]}
	replacement, err := reduceToolRound(ctx, store, scope, round)
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	splice := func(messages []*schema.AgenticMessage) []*schema.AgenticMessage {
		out := make([]*schema.AgenticMessage, 0, len(messages)-2+len(replacement))
		out = append(out, messages[:2]...)
		out = append(out, replacement...)
		out = append(out, messages[4:]...)
		return out
	}
	postTarget := limits.SummarizeAt + min(int64(1000), max(int64(64), (limits.Hard-limits.SummarizeAt)/4))
	if postTarget >= limits.Hard {
		postTarget = limits.Hard - 1
	}
	low, high := 0, int(limits.Hard)
	for low < high {
		mid := low + (high-low)/2
		candidate := build(mid)
		reduced := splice(candidate)
		post, err := inputbudget.EstimateAgenticInput(reduced, nil, nil, nil, nil, 2)
		if err != nil {
			return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
		}
		if post.TotalBytes < postTarget {
			low = mid + 1
		} else {
			high = mid
		}
	}
	messages := build(low)
	reduced := splice(messages)
	estimate, err := inputbudget.EstimateAgenticInput(messages, nil, nil, nil, nil, 2)
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	post, err := inputbudget.EstimateAgenticInput(reduced, nil, nil, nil, nil, 2)
	if err != nil {
		return nil, inputbudget.Estimate{}, inputbudget.Estimate{}, err
	}
	if estimate.TotalBytes >= limits.Hard || estimate.TotalBytes <= limits.SummarizeAt || post.TotalBytes <= limits.SummarizeAt {
		return nil, estimate, post, inputbudget.ErrInvalidLimits
	}
	return messages, estimate, post, nil
}

func liveCompressionToolCall(id, sourceID, part string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: "read_material", Arguments: `{"material_id":"` + sourceID + `","part":"` + part + `"}`}),
	}}
}

func liveCompressionToolResult(id, name, value string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.FunctionToolResult{CallID: id, Name: name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: value}}}}),
	}}
}

func liveCompressionAssistantMessage(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: text})}}
}

func cloneLiveCompressionMessages(t *testing.T, messages []*schema.AgenticMessage) []*schema.AgenticMessage {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal("cannot clone fictional history")
	}
	var clone []*schema.AgenticMessage
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal("cannot clone fictional history")
	}
	return clone
}

func hasLiveCompressionToolCall(message *schema.AgenticMessage) bool {
	if message == nil {
		return false
	}
	for _, block := range message.ContentBlocks {
		if block != nil && block.FunctionToolCall != nil {
			return true
		}
	}
	return false
}

func liveCompressionAnswerCorrect(message *schema.AgenticMessage, code, sourceID string) bool {
	if message == nil {
		return false
	}
	var text strings.Builder
	for _, block := range message.ContentBlocks {
		if block != nil && block.AssistantGenText != nil {
			text.WriteString(block.AssistantGenText.Text)
		}
	}
	answer := text.String()
	return strings.TrimSpace(answer) == "decision=APPROVE; verification_code="+code+"; source_id="+sourceID
}

func hasUsage(record inputbudget.ModelRecord) bool {
	return record.InputTokens != 0 || record.OutputTokens != 0 || record.TotalTokens != 0
}

type liveCompressionReadTraceStore struct {
	*Store
	reads int
}

func (s *liveCompressionReadTraceStore) ReadMaterial(ctx context.Context, scope ReadScope, id, part string, request textpage.Request) (MaterialText, error) {
	s.reads++
	return s.Store.ReadMaterial(ctx, scope, id, part, request)
}
