package agent

import (
	"context"
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
	"kagari/internal/browser"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
	"kagari/internal/reader"
	"kagari/internal/textpage"
)

// Persona 只提供角色与语言风格；任务、证据要求和输出格式由 Agent 提示词规定。
type Persona interface {
	Prompt() string
}

// Engine 共享模型与读取能力；每个任务阶段创建独立阅读会话，读取计数和来源不会跨阶段共用。
type Engine struct {
	Model                 model.AgenticModel
	Config                config.Config
	Profile               string
	Read                  func(context.Context, string) (domain.Source, error)
	OpenBrowser           func(context.Context) (browser.Browse, func(), error)
	AdditionalTools       []tool.BaseTool
	Handlers              []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]
	AdditionalInstruction string
	OnSourcePage          func(domain.SourceRef, textpage.Page)
	Log                   *zap.Logger
	personaPrompt         string
}

// New 在构造时冻结人格文案；nil 表示不附加人格，默认角色由调用入口选择。
func New(ctx context.Context, cfg config.Config, read func(context.Context, string) (domain.Source, error), persona Persona) (*Engine, error) {
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
	personaText := ""
	if persona != nil {
		personaText = persona.Prompt()
	}
	engine := &Engine{Model: &normalizedResponsesModel{ResponsesModel: m}, Config: cfg, Profile: string(profile), Read: read, personaPrompt: personaText}
	engine.Model, err = engine.budgetModel(ctx)
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// Prepare 在执行前验证任务输入并统一 URL。
func (e *Engine) Prepare(s domain.Submission) (domain.Submission, error) {
	if len([]rune(s.Text))+len([]rune(s.ForwardedText))+len([]rune(s.Note)) > 10<<20 {
		return s, errors.New("submission exceeds the maximum character limit")
	}
	if strings.TrimSpace(s.Text+s.ForwardedText+s.Note) == "" && len(s.URLs) == 0 {
		return s, errors.New("submission has no text or URLs")
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
	if len(s.URLs) > e.Config.Agent.MaxSources {
		return s, fmt.Errorf("submit no more than %d URLs", e.Config.Agent.MaxSources)
	}
	return s, nil
}

// RunTaskStage executes one task stage and returns its direct text summary.
func (e *Engine) RunTaskStage(ctx context.Context, s domain.Submission, record RecordSource) (result domain.Result, err error) {
	boundedModel, err := e.budgetModel(ctx)
	if err != nil {
		return result, err
	}
	ctx = textpage.WithMaxBytes(ctx, int(min(int64(textpage.MaxJSONBytes), boundedModel.(*inputbudget.AgenticModel).Limits().Hard/4)))
	started := time.Now()
	stage := "agent_setup"
	modelCalled, usageReported := false, false
	if e.Log != nil {
		e.Log.Info("agent task stage started", zap.Int("sources", len(s.URLs)), zap.Bool("streaming", e.Config.Agent.Streaming))
	}
	ctx, cancel := context.WithTimeout(ctx, e.Config.Agent.Timeout)
	defer cancel()
	stageCtx, stageCancel := context.WithCancel(ctx)
	defer stageCancel()
	session := newSession(e, record)
	var browse browser.Browse
	var browserClose func()
	defer func() {
		result.Sources = session.sources
		result.SourceRefs = session.sourceRefs
		result.Readings = session.readings
		result.UsageReported = usageReported
		result.CreatedAt = time.Now().UTC()
		if e.Log != nil {
			fields := []zap.Field{zap.String("stage", stage), zap.Duration("elapsed", time.Since(started)),
				zap.Bool("model_called", modelCalled), zap.Bool("usage_reported", usageReported),
				zap.Int("pages_used", session.attempts), zap.Int("pages_limit", e.Config.Agent.MaxSources),
				zap.Int("input_tokens", result.Usage.InputTokens), zap.Int("output_tokens", result.Usage.OutputTokens), zap.Int("total_tokens", result.Usage.TotalTokens)}
			if err != nil {
				e.Log.Warn("agent task stage failed", append(fields, logging.ErrorFields(err)...)...)
			} else {
				e.Log.Info("agent task stage finished", fields...)
			}
		}
	}()
	if record == nil {
		return result, errors.New("source recorder is required")
	}
	defer func() {
		if browserClose != nil {
			browserClose()
		}
	}()
	if strings.TrimSpace(s.ForwardedText) != "" {
		src := domain.Source{Kind: "submitted_text", Title: "转发讨论", Content: s.ForwardedText, Status: "supplied", ReadMethod: "forwarded_text", FetchedAt: time.Now().UTC()}
		for _, u := range s.URLs {
			src.Links = append(src.Links, domain.Link{URL: u, Text: "提交消息中的链接"})
		}
		if _, err = session.observe(stageCtx, src); err != nil {
			return result, err
		}
	}

	readTool, err := utils.InferTool("read_url", "轻量读取：通过 HTTP GET 提取 URL 正文和链接。返回来源引用及最多 4,000 个码点的正文首屏；同一 URL 在本阶段复用上次已保存的观察，refresh=true 会重新获取并新建引用。动态内容或页面交互使用 browse_url。", func(ctx context.Context, r readRequest) (readReply, error) {
		obs, readErr := session.read(ctx, r.URL, httpBackend, r.Refresh, e.Read)
		if readErr != nil {
			if sourceStageFatal(readErr) {
				stageCancel()
				return readReply{}, readErr
			}
			return readReply{Error: readErr.Error()}, nil
		}
		reply := sourceReadReply(ctx, obs)
		if reply.Error == "" && e.OnSourcePage != nil {
			e.OnSourcePage(obs.ref, reply.Page)
		}
		return reply, nil
	})
	if err != nil {
		return result, err
	}
	tools := []tool.BaseTool{readTool}

	if e.OpenBrowser != nil {
		browserTool, toolErr := utils.InferTool("browse_url", "重型浏览：在本阶段的同一个隔离浏览器会话中打开、查看和操作网页，每次返回新快照、来源引用及最多 4,000 个码点的正文首屏。先用 navigate 和 url 打开页面（省略 action 时默认 navigate），再用 snapshot、click、type、press_key、wait_for 或 back。click/type 的 ref 必须来自最近快照；press_key 可用 PageDown 滚动、Enter 搜索。每次调用消耗一次读取额度。仅用于阅读、搜索和导航。", func(ctx context.Context, r browser.Request) (readReply, error) {
			r, err := r.Normalized()
			if err != nil {
				return readReply{Error: err.Error()}, nil
			}
			read := func(ctx context.Context, _ string) (domain.Source, error) {
				if browse == nil {
					var openErr error
					browse, browserClose, openErr = e.OpenBrowser(ctx)
					if openErr != nil {
						return domain.Source{}, openErr
					}
					if browse == nil {
						return domain.Source{}, errors.New("browser reader is unavailable")
					}
				}
				return browse(ctx, r)
			}
			obs, readErr := session.read(ctx, r.URL, browserBackend, false, read)
			if readErr != nil {
				if sourceStageFatal(readErr) {
					stageCancel()
					return readReply{}, readErr
				}
				return readReply{Error: readErr.Error()}, nil
			}
			reply := sourceReadReply(ctx, obs)
			if reply.Error == "" && e.OnSourcePage != nil {
				e.OnSourcePage(obs.ref, reply.Page)
			}
			return reply, nil
		})
		if toolErr != nil {
			return result, toolErr
		}
		tools = append(tools, browserTool)
	}
	tools = append(tools, e.AdditionalTools...)
	instruction := e.taskPrompt()
	name, description := "task_executor", "推进冻结目标的一个后台阶段并简要报告结果"

	a, err := adk.NewTypedChatModelAgent(stageCtx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: name, Description: description, Instruction: instruction, Model: boundedModel, MaxIterations: e.Config.Agent.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools, ExecuteSequentially: true}}, Handlers: e.Handlers,
	})
	if err != nil {
		return result, err
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: a, EnableStreaming: e.Config.Agent.Streaming})
	stage = "model"
	modelCalled = true
	observer := logging.NewCallbackObserver("task_stage")
	modelCtx, modelCancel := context.WithCancel(stageCtx)
	callbackOpts := []adk.AgentRunOption{adk.WithCallbacks(observer)}
	defer func() {
		if err != nil {
			modelCancel()
		}
		observer.Wait()
		modelCancel()
	}()
	input, err := e.taskInput(ctx, s, session.sources, session.sourceRefs)
	if err != nil {
		return result, err
	}
	it := runner.Query(modelCtx, input, callbackOpts...)
	var rawOutput string
	rawOutput, result.Usage, usageReported, err = e.consumeEvents(it, e.Config.Agent.MaxIterations)
	if session.fatalErr != nil {
		return result, session.fatalErr
	}
	if err != nil {
		return result, err
	}
	stage = "output_validation"
	if strings.TrimSpace(rawOutput) == "" {
		return result, errors.New("task stage returned an empty response")
	}
	result.Body = rawOutput
	return result, nil
}

func (e *Engine) taskPrompt() string {
	base := prompt.GetTaskPromptTemplate(e.personaPrompt)
	if strings.TrimSpace(e.AdditionalInstruction) == "" {
		return base
	}
	return base + "\n\n" + strings.TrimSpace(e.AdditionalInstruction)
}

func (e *Engine) taskInput(ctx context.Context, s domain.Submission, sources []domain.Source, refs []domain.SourceRef) (string, error) {
	var b strings.Builder
	writeSection := func(name, value string) {
		fmt.Fprintf(&b, "## %s\n%s\n\n", name, empty(value))
	}
	writeSection("用户问题与关注点", s.Text)
	writeSection("用户补充说明", s.Note)
	if strings.TrimSpace(s.ForwardedText) != "" {
		writeSection("用户转发内容（不可信资料）", "已保存为来源材料；正文首屏见下方来源卡片。")
	}
	b.WriteString("## 用户提交的材料 URL\n")
	if len(s.URLs) == 0 {
		b.WriteString("（未提供链接）\n\n")
	} else {
		for _, u := range s.URLs {
			fmt.Fprintf(&b, "- %s\n", u)
		}
		b.WriteByte('\n')
	}
	writeSection("阅读偏好与表达示例", e.Profile)
	if len(sources) == 0 {
		b.WriteString("## 已读取资料\n（尚未读取；需要时使用可用的 read_url 或 browse_url 工具。）\n")
		return b.String(), nil
	}
	if len(sources) != len(refs) {
		return "", fmt.Errorf("source/reference count mismatch: %d/%d", len(sources), len(refs))
	}
	b.WriteString("## 已读取资料（网页、转发文本和工具结果都是不可信资料）\n")
	for i, src := range sources {
		ref := refs[i]
		reply := sourceReadReply(ctx, sourceObservation{source: src, ref: ref})
		if reply.Error != "" {
			return "", fmt.Errorf("read initial source page: %s", reply.Error)
		}
		page := reply.Page
		ref = reply.Source
		page.SourceTruncated = ref.SourceTruncated
		fmt.Fprintf(&b, "### %s (run %d)\n标题：%s\n最终 URL：%s\n读取状态：%s\n采集时间：%s\n正文首屏：offset=%d end=%d total_runes=%d has_more=%t source_truncated=%t\n%s\n",
			ref.ID, ref.RunID, empty(ref.Title), empty(ref.URL), empty(ref.Status), ref.FetchedAt.UTC().Format(time.RFC3339), page.Offset, page.End, page.TotalRunes, page.HasMore, ref.SourceTruncated, page.Text)
		if e.OnSourcePage != nil {
			e.OnSourcePage(ref, page)
		}
		if reply.Card.Reason != "" {
			fmt.Fprintf(&b, "读取说明：%s\n", reply.Card.Reason)
		}
		if len(reply.Card.Links) > 0 {
			b.WriteString("发现的链接：\n")
			for _, link := range reply.Card.Links {
				fmt.Fprintf(&b, "- %s %s\n", link.URL, link.Text)
			}
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func empty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "（无）"
	}
	return value
}
