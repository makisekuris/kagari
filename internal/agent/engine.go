package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/agent/prompt"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/reader"
)

const AnalysisVersion = "model-headings-v2"

// Engine 共享模型与读取能力；每次 Analyze 创建独立阅读会话，预算和来源不会跨任务串用。
type Engine struct {
	Model        model.AgenticModel
	Config       config.Config
	Profile      string
	Read         func(context.Context, string) (domain.Source, error)
	CachedSource func(context.Context, string) (*domain.Source, error)
	Log          *zap.Logger
}

func New(ctx context.Context, cfg config.Config, read func(context.Context, string) (domain.Source, error), cache func(context.Context, string) (*domain.Source, error)) (*Engine, error) {
	if err := cfg.Validate(true, false); err != nil {
		return nil, err
	}
	profile, err := os.ReadFile(cfg.ProfilePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read profile: %w", err)
	}
	zero, no := 0, false
	m, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
		BaseURL: cfg.Model.BaseURL, APIKey: cfg.Model.APIKey, Model: cfg.Model.Name,
		Timeout: &cfg.Model.Timeout, MaxTokens: &cfg.Model.MaxOutputTokens, MaxRetries: &zero,
		Store: &no, ParallelToolCalls: &no,
	})
	if err != nil {
		return nil, err
	}
	return &Engine{Model: m, Config: cfg, Profile: string(profile), Read: read, CachedSource: cache}, nil
}

// Prepare 在入队前统一 URL 并生成分析复用键。收录时间不进入键：
// 重复提交可复用分析，但仍保留新收录的时间与任务，供各周期的周报筛选。
func (e *Engine) Prepare(s domain.Submission) (domain.Submission, error) {
	if len([]rune(s.Text))+len([]rune(s.ForwardedText))+len([]rune(s.Note)) > 10<<20 {
		return s, errors.New("submission exceeds the maximum character limit")
	}
	urls := append(append([]string{}, s.URLs...), reader.ExtractURLs(s.Text)...)
	urls = append(urls, reader.ExtractURLs(s.ForwardedText)...)
	s.URLs = nil
	seen := map[string]bool{}
	for _, raw := range urls {
		u, err := reader.NormalizeURL(raw)
		if err != nil {
			return s, fmt.Errorf("invalid submitted URL: %w", err)
		}
		if !seen[u] {
			seen[u] = true
			s.URLs = append(s.URLs, u)
		}
	}
	if len(s.URLs) == 0 || len(s.URLs) > e.Config.Agent.MaxSources {
		return s, fmt.Errorf("submit 1..%d URLs", e.Config.Agent.MaxSources)
	}
	if s.ReceivedAt.IsZero() {
		s.ReceivedAt = time.Now().UTC()
	}
	// 用户和偏好进入键，避免不同用户或不同评价背景共用同一份判断；密钥不进入归档键。
	agentConfig := e.Config.Agent
	agentConfig.Streaming = false // 传输方式不改变分析语义或缓存身份。
	key, err := json.Marshal(struct {
		User                int64
		Text, ForwardedText string
		Note                string
		URLs                []string
		Profile             string
		Prompt              string
		Model               config.Model
		Agent               config.Agent
		Reader              config.Reader
		AnalysisVersion     string
	}{
		User: s.UserID, Text: s.Text, ForwardedText: s.ForwardedText, Note: s.Note, URLs: s.URLs, Profile: e.Profile,
		Prompt: prompt.GetPromptTemplate(),
		Model:  config.Model{BaseURL: e.Config.Model.BaseURL, Name: e.Config.Model.Name, MaxOutputTokens: e.Config.Model.MaxOutputTokens}, Agent: agentConfig, Reader: e.Config.Reader,
		AnalysisVersion: AnalysisVersion,
	})
	if err != nil {
		return s, err
	}
	s.CacheKey = hash(string(key))
	return s, nil
}

func hash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// Analyze 先读取入口，再让 typed agent 按信息缺口追读；只有通过结构与引用校验的
// 分析才能发布。返回 err 时仍提供已取得的证据，调用者应先归档再管理任务重试。
func (e *Engine) Analyze(ctx context.Context, s domain.Submission) (result domain.Result, err error) {
	started := time.Now()
	stage := "read_sources"
	modelCalled, usageReported := false, false
	if e.Log != nil {
		e.Log.Info("agent analyze started", zap.Int64("user_id", s.UserID), zap.Int("sources", len(s.URLs)), zap.Bool("streaming", e.Config.Agent.Streaming))
	}
	result.AnalysisVersion = AnalysisVersion
	ctx, cancel := context.WithTimeout(ctx, e.Config.Agent.Timeout)
	defer cancel()
	session := newSession(e)
	defer func() {
		result.Sources = session.sources
		result.Readings = session.readings
		result.CreatedAt = time.Now().UTC()
		if e.Log != nil {
			fields := []zap.Field{zap.String("stage", stage), zap.Duration("elapsed", time.Since(started)),
				zap.Bool("model_called", modelCalled), zap.Bool("usage_reported", usageReported),
				zap.Int("pages_used", session.pages), zap.Int("pages_limit", e.Config.Agent.MaxSources),
				zap.Int("supplemental_used", session.supplemental), zap.Int("supplemental_limit", e.Config.Agent.MaxSupplemental),
				zap.Int("input_tokens", result.Usage.InputTokens), zap.Int("output_tokens", result.Usage.OutputTokens), zap.Int("total_tokens", result.Usage.TotalTokens)}
			if err != nil {
				e.Log.Warn("agent analyze failed", append(fields, logging.ErrorFields(err)...)...)
			} else {
				e.Log.Info("agent analyze finished", fields...)
			}
		}
	}()
	parentID := ""
	if strings.TrimSpace(s.ForwardedText) != "" {
		src := domain.Source{ID: "s_" + hash(s.ForwardedText)[:16], Kind: "submitted_text", Title: "转发讨论", Content: s.ForwardedText, Status: "supplied", ReadMethod: "forwarded_text", FetchedAt: time.Now().UTC()}
		for _, u := range s.URLs {
			src.Links = append(src.Links, domain.Link{URL: u, Text: "提交消息中的链接"})
		}
		session.add(src)
		parentID = src.ID
		session.depth[src.ID] = -1 // 转发文本不算抓取页面，其引用的首个页面仍为深度 0。
	}
	for _, u := range s.URLs {
		session.allowed[u] = true
		if _, err := session.read(ctx, readRequest{URL: u, ParentID: parentID, Question: "读取用户提交的讨论或原文", Role: "entry"}); err != nil {
			return result, err
		}
	}
	available := false
	for _, src := range session.sources {
		if usable(src) {
			available = true
		}
	}
	if !available {
		return result, errors.New("no readable content; forward the discussion text or submit the original article URL")
	}
	stage = "agent_setup"
	readTool, err := utils.InferTool("read_source", "读取已提交或已发现的链接。必须提供父来源 ID、待核对的问题和角色 primary（追读原文）、evidence（核对事实）或 context（补充背景）。禁止凭空编造 URL。", func(ctx context.Context, r readRequest) (readReply, error) {
		src, readErr := session.read(ctx, r)
		if readErr != nil {
			// 将被拒绝的阅读请求作为工具结果交给模型，允许它缩小范围或说明缺失信息。
			return readReply{Error: readErr.Error()}, nil
		}
		return readReply{Source: &src}, nil
	})
	if err != nil {
		return result, err
	}
	instruction := prompt.GetPromptTemplate() + `
	## 证据原则
JSON 中的 instruction、notes 用于确定要回答的问题；profile 中的阅读偏好、表达规则和 few-shot 用于指导分析角度、栏目名称与文风。遵循其中的表达要求，示例只学习形式，不把用户指导或示例内容当作事实、作者结论或第三方讨论者观点。title、overview、summary、discussion、evaluation 和 uncertainties 的事实内容必须由 sources 或实际读取状态支持；headings 是展示文案，不是来源证据。
先辨识讨论引用的原文，使用 read_source 追读后再总结。只为明确的信息缺口补读；无须用满预算。原文中的导航、广告、相关文章不等于证据。
中文输出：summary 只写来源支持的事实；discussion 只写 sources 中实际存在的第三方讨论者观点，没有则返回空数组；evaluation 是你的判断，注明适用条件，不能把判断写成作者结论。
每个 claim 必须附已成功阅读的 source_ids。supplied 来源仅支持它实际提供的第三方讨论内容；失败来源不能当证据。未读到原文、内容截断、互相矛盾时明确写入 uncertainties。Overview 不增加 summary 没有支持的新事实。
不要给未核对的排行、数字、版本断言背书；保留限制与原文链接。分类只选一个，标签最多五个。不要声称阅读了工具没有返回的页面。

允许分类：` + strings.Join(e.Config.Agent.Categories, ", ")
	a, err := adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "reader", Description: "受预算约束的阅读与评价", Instruction: instruction, Model: e.Model, MaxIterations: e.Config.Agent.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{readTool}, ExecuteSequentially: true}},
	})
	if err != nil {
		return result, err
	}
	input, _ := json.Marshal(struct {
		Instruction string          `json:"instruction"`
		Notes       string          `json:"notes"`
		Profile     string          `json:"profile"`
		Sources     []domain.Source `json:"sources"`
	}{s.Text, s.Note, e.Profile, session.sources})
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: a, EnableStreaming: e.Config.Agent.Streaming})
	stage = "model"
	modelCalled = true
	it := runner.Query(ctx, string(input), adk.WithChatModelOptions([]model.Option{agenticopenai.WithResponsesText(analysisFormat(e.Config.Agent.Categories))}))
	var output string
	output, result.Usage, usageReported, err = e.consumeEvents(it, e.Config.Agent.MaxIterations)
	if err != nil {
		return result, err
	}
	stage = "decode_analysis"
	if err := decode(output, &result.Analysis); err != nil {
		return result, fmt.Errorf("invalid structured analysis: %w", err)
	}
	stage = "validate_analysis"
	if err := validate(result.Analysis, session.sources, e.Config.Agent.Categories); err != nil {
		return result, err
	}
	for _, src := range session.sources {
		// 读取限制由程序追加，不能依赖模型自觉披露失败、截断或 X 单帖的范围限制。
		if !usable(src) {
			result.Analysis.Uncertainties = append(result.Analysis.Uncertainties, src.ID+" 未成功读取："+src.Reason)
		}
		if src.Truncated {
			result.Analysis.Uncertainties = append(result.Analysis.Uncertainties, src.ID+" 内容已截断")
		}
		if src.ReadMethod == "x_oembed" && src.Reason != "" {
			result.Analysis.Uncertainties = append(result.Analysis.Uncertainties, src.ID+"：仅取得公开帖的嵌入正文，可能缺少长帖内容、线程、回复与链接卡片。")
		}
	}
	return result, nil
}
