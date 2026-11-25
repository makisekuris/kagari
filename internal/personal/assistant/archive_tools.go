package assistant

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/channel"
	"kagari/internal/documentarchive"
	"kagari/internal/personal"
	"kagari/internal/textpage"
)

const (
	archiveSearchLimit   = 5
	archiveHistoryLimit  = 10
	archiveMaxQueryRunes = 256
	archiveMaxTitleRunes = 200
	archiveMaxNoteRunes  = 2_000
)

var archiveNumericID = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_])%d(?:$|[^\p{L}\p{N}_])`)

type archiveAttachmentArgs struct {
	AttachmentID    string   `json:"attachment_id" jsonschema:"description=ID of an attachment present in the current message, reply target, recent conversation history, or pending user input."`
	Title           string   `json:"title,omitempty" jsonschema:"description=Title for a new document. Leave empty to use the source filename."`
	DocumentID      int64    `json:"document_id,omitempty" jsonschema:"description=Existing document ID returned by an archive search/detail tool or stated by the user."`
	Category        string   `json:"category,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Note            string   `json:"note,omitempty" jsonschema:"description=Only a note explicitly supplied in this message. The text must appear verbatim in the current user message."`
	Number          int      `json:"number,omitempty" jsonschema:"description=Only when the current user explicitly identifies the version number; otherwise leave zero for automatic numbering."`
	Historical      bool     `json:"historical,omitempty"`
	VersionEvidence string   `json:"version_evidence,omitempty" jsonschema:"description=Exact current-message quote identifying an explicitly supplied version number."`
}

type archiveSearchArgs struct {
	Query    string    `json:"query,omitempty"`
	Category string    `json:"category,omitempty"`
	Tag      string    `json:"tag,omitempty"`
	From     time.Time `json:"from,omitempty"`
	To       time.Time `json:"to,omitempty"`
	Limit    int       `json:"limit,omitempty" jsonschema:"description=At most five results per call."`
	Offset   int       `json:"offset,omitempty"`
}

type archiveGetArgs struct {
	DocumentID   int64 `json:"document_id,omitempty"`
	VersionID    int64 `json:"version_id,omitempty"`
	BeforeNumber int   `json:"before_number,omitempty"`
	Limit        int   `json:"limit,omitempty"`
}

type archiveVersionArgs struct {
	VersionID int64 `json:"version_id"`
}

type archiveReadTextArgs struct {
	VersionID int64 `json:"version_id"`
	textpage.Request
}

type archiveVersionCard struct {
	documentarchive.Version
	Archived bool `json:"archived"`
}

func versionCard(version documentarchive.Version) archiveVersionCard {
	return archiveVersionCard{Version: version, Archived: version.Archived()}
}

type archiveHitCard struct {
	Document     documentarchive.Document `json:"document"`
	Match        archiveVersionCard       `json:"match"`
	Latest       archiveVersionCard       `json:"latest"`
	MatchedField string                   `json:"matched_field,omitempty"`
}

type archiveDetailCard struct {
	Document         documentarchive.Document `json:"document"`
	Latest           archiveVersionCard       `json:"latest"`
	Versions         []archiveVersionCard     `json:"versions"`
	NextBeforeNumber int                      `json:"next_before_number,omitempty"`
}

type archiveToolReply struct {
	OK         bool                      `json:"ok"`
	Error      string                    `json:"error,omitempty"`
	Document   *documentarchive.Document `json:"document,omitempty"`
	Version    *archiveVersionCard       `json:"version,omitempty"`
	Detail     *archiveDetailCard        `json:"detail,omitempty"`
	Hits       []archiveHitCard          `json:"hits"`
	Download   *archiveDownloadCard      `json:"download,omitempty"`
	Original   *documentarchive.Original `json:"original,omitempty"`
	Text       *documentarchive.Text     `json:"text,omitempty"`
	HasMore    bool                      `json:"has_more,omitempty"`
	Offset     int                       `json:"offset,omitempty"`
	NextOffset int                       `json:"next_offset,omitempty"`
}

type archiveDownloadCard struct {
	VersionID   int64                  `json:"version_id"`
	UploadState documentarchive.Status `json:"upload_status"`
	Ready       bool                   `json:"ready"`
	ExpiresAt   time.Time              `json:"expires_at,omitempty"`
}

type archiveAttachmentSource struct {
	attachment    channel.Attachment
	sourceEventID int64
}

// archiveTurnTools owns the user-visible side effects for one MainAgent turn.
// Signed URLs are kept here until after model generation and tool logging finish.
type archiveTurnTools struct {
	service             *documentarchive.Service
	files               channel.Files
	input               personal.TurnInput
	event               personal.Event
	createdAttachment   func(channel.Attachment)
	knownAttachments    map[string]archiveAttachmentSource
	knownDocuments      map[int64]bool
	knownVersions       map[int64]bool
	mandatoryReceipts   map[string]string
	receiptOrder        []string
	downloads           []documentarchive.Download
	actions             *personalTools
	preparedAttachments []channel.Attachment
}

func newArchiveTurnTools(service *documentarchive.Service, files channel.Files, input personal.TurnInput, actions *personalTools, created func(channel.Attachment)) *archiveTurnTools {
	c := &archiveTurnTools{
		service: service, files: files, input: input, event: input.Event, createdAttachment: created,
		knownAttachments: make(map[string]archiveAttachmentSource), knownDocuments: make(map[int64]bool), knownVersions: make(map[int64]bool),
		mandatoryReceipts: make(map[string]string),
		actions:           actions,
	}
	c.collectAttachmentContext()
	return c
}

func (c *archiveTurnTools) collectAttachmentContext() {
	add := func(file channel.Attachment, sourceEventID int64) {
		if strings.TrimSpace(file.ID) == "" {
			return
		}
		current, exists := c.knownAttachments[file.ID]
		// Prefer a source input event over a file that appeared only in an
		// assistant delivery. This keeps provenance tied to original user input.
		if !exists || current.sourceEventID == 0 && sourceEventID > 0 {
			c.knownAttachments[file.ID] = archiveAttachmentSource{attachment: file, sourceEventID: sourceEventID}
		}
	}
	addInput := func(event personal.Event) {
		if event.Kind != personal.EventUser {
			return
		}
		for _, file := range event.Attachments {
			add(file, event.ID)
		}
	}
	addInput(c.event)
	for _, event := range c.input.PendingInputs {
		addInput(event)
	}
	for _, event := range c.input.PendingTaskInputs {
		addInput(event)
	}
	if c.input.RepliedMessage != nil && c.input.RepliedMessage.Role == "user" && c.input.RepliedMessage.EventID > 0 {
		message := c.input.RepliedMessage
		for _, file := range message.Attachments {
			add(file, message.EventID)
		}
	}
	for _, message := range c.input.History {
		if message.Role != "user" || message.EventID <= 0 {
			continue
		}
		for _, file := range message.Attachments {
			add(file, message.EventID)
		}
	}
}

func (c *archiveTurnTools) build() ([]tool.BaseTool, error) {
	archive, err := utils.InferTool("archive_attachment", "仅当当前直接用户消息明确要求归档时调用。attachment_id 必须来自当前消息、回复目标、最近历史或待处理用户输入中的附件。document_id 必须来自本轮归档搜索/详情结果，或由用户在当前消息中明确给出。新文档标题可用原文件名。只有用户明确指定某个版本号时才填写 number，并在 version_evidence 中引用当前消息原文；普通新提交保持 historical=false。只有用户明确补录更旧版本时才同时填写 historical=true，且该历史位置必须低于现有最新版。note 只能逐字取自用户当前消息。category/tags 仅在新建文档时使用，且只采用用户明确提供的信息。请求成功后会返回实际原件与简述状态，回复必须如实说明‘已归档’或‘已登记/处理中’。", func(ctx context.Context, args archiveAttachmentArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		source, ok := c.knownAttachments[strings.TrimSpace(args.AttachmentID)]
		if !ok {
			return c.failure(errors.New("attachment_id is not in the current or recent user conversation context")), nil
		}
		if source.sourceEventID <= 0 {
			return c.failure(errors.New("attachment has no matching user input event")), nil
		}
		if args.DocumentID < 0 || args.Number < 0 {
			return c.failure(errors.New("document_id and number must be nonnegative")), nil
		}
		if args.Historical && (args.DocumentID == 0 || args.Number <= 0) {
			return c.failure(errors.New("a historical version requires an existing document, a positive number, and historical=true")), nil
		}
		if args.DocumentID > 0 && !c.knowsDocument(args.DocumentID) {
			return c.failure(errors.New("document_id must be returned by an archive search/detail tool or stated by the current user")), nil
		}
		if args.Number > 0 && (strings.TrimSpace(args.VersionEvidence) == "" || !strings.Contains(c.event.Text, args.VersionEvidence)) {
			return c.failure(errors.New("an explicit version number requires an exact quote from the current user message")), nil
		}
		note := strings.TrimSpace(args.Note)
		if note != "" {
			if !strings.Contains(c.event.Text, note) || utf8.RuneCountInString(note) > archiveMaxNoteRunes {
				return c.failure(errors.New("note must be text from the current user message")), nil
			}
		}
		title := strings.TrimSpace(args.Title)
		if title == "" && args.DocumentID == 0 {
			title = filepath.Base(source.attachment.Name)
		}
		if args.DocumentID == 0 && title == "" || utf8.RuneCountInString(title) > archiveMaxTitleRunes {
			return c.failure(errors.New("a document title of at most 200 characters is required")), nil
		}
		category, tags := "", []string(nil)
		if args.DocumentID == 0 {
			category, tags = strings.TrimSpace(args.Category), normalizedArchiveTags(args.Tags)
		}
		if err := c.validateMetadata(category, tags); err != nil {
			return c.failure(err), nil
		}
		request := documentarchive.Request{
			EventID: c.event.ID, AttachmentID: strings.TrimSpace(args.AttachmentID), SourceEventID: source.sourceEventID,
			DocumentID: args.DocumentID, Title: title, Category: category,
			Tags: tags, Note: note, Number: args.Number, Historical: args.Historical,
		}
		if c.actions == nil {
			return c.failure(errors.New("archive action key generator is not configured")), nil
		}
		requestKey, err := c.actions.requestKey("archive_attachment", request)
		if err != nil {
			return c.failure(errors.New("could not prepare an idempotent archive request")), nil
		}
		request.RequestKey = requestKey
		version, err := c.service.Enqueue(ctx, request)
		if err != nil {
			c.addMandatoryReceipt(request.RequestKey, "归档请求未能确认成功，当前没有可用的归档回执。")
			return c.failure(archiveSafeError(err)), nil
		}
		c.knownDocuments[version.DocumentID] = true
		c.knownVersions[version.ID] = true
		card := versionCard(version)
		c.addMandatoryReceipt(request.RequestKey, archiveReceipt(version))
		return archiveToolReply{OK: true, Version: &card}, nil
	})
	if err != nil {
		return nil, err
	}

	search, err := utils.InferTool("search_archives", "搜索或列出归档目录中的标题、标识符、文件名、标签、备注和简述。省略查询与过滤条件可列出最近归档。每次最多返回五个候选；结果会包含命中的版本、匹配依据和独立的最新版状态。先搜索并查看简述，再按需读取指定版本原件或文字。", func(ctx context.Context, args archiveSearchArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		queryText := strings.TrimSpace(args.Query)
		category, tag := strings.TrimSpace(args.Category), strings.TrimSpace(args.Tag)
		if utf8.RuneCountInString(queryText) > archiveMaxQueryRunes || utf8.RuneCountInString(category) > 128 || utf8.RuneCountInString(tag) > 128 {
			return c.failure(errors.New("search filters exceed their size limits")), nil
		}
		limit := args.Limit
		if limit == 0 {
			limit = archiveSearchLimit
		}
		if limit < 1 || limit > archiveSearchLimit || args.Offset < 0 || args.Offset > 1_000 {
			return c.failure(errors.New("search limit must be 1–5 and offset must be between 0 and 1000")), nil
		}
		if !args.From.IsZero() && !args.To.IsZero() && !args.From.Before(args.To) {
			return c.failure(errors.New("from must be earlier than to")), nil
		}
		hits, err := c.service.Search(ctx, documentarchive.Query{Text: queryText, Category: category, Tag: tag, From: args.From, To: args.To, Limit: limit + 1, Offset: args.Offset})
		if err != nil {
			return c.failure(archiveSafeError(err)), nil
		}
		hasMore := len(hits) > limit
		hits = hits[:min(limit, len(hits))]
		cards := make([]archiveHitCard, 0, len(hits))
		for _, hit := range hits {
			document := clipArchiveDocument(hit.Document)
			match, latest := clipArchiveVersion(hit.Match), clipArchiveVersion(hit.Latest)
			c.knownDocuments[hit.Document.ID] = true
			c.knownVersions[hit.Match.ID] = true
			c.knownVersions[hit.Latest.ID] = true
			cards = append(cards, archiveHitCard{Document: document, Match: versionCard(match), Latest: versionCard(latest), MatchedField: hit.MatchedField})
		}
		reply := archiveToolReply{OK: true, Hits: cards, HasMore: hasMore, Offset: args.Offset}
		if hasMore {
			reply.NextOffset = args.Offset + len(cards)
		}
		bounded, err := boundArchiveToolReply(ctx, reply)
		if err != nil {
			return c.failure(errors.New("search results exceed the response limit; request fewer results")), nil
		}
		return bounded, nil
	})
	if err != nil {
		return nil, err
	}

	get, err := utils.InferTool("get_archive", "按 document_id 查看有界版本历史，或按 version_id 查看单版记录和其归档状态。document_id/version_id 必须来自搜索、先前详情结果或用户明确给出的标识符。", func(ctx context.Context, args archiveGetArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		if (args.DocumentID > 0) == (args.VersionID > 0) || args.DocumentID < 0 || args.VersionID < 0 {
			return c.failure(errors.New("provide exactly one positive document_id or version_id")), nil
		}
		if args.DocumentID > 0 {
			if !c.knowsDocument(args.DocumentID) {
				return c.failure(errors.New("document_id must be returned by an archive search/detail tool or stated by the current user")), nil
			}
			limit := args.Limit
			if limit == 0 {
				limit = archiveHistoryLimit
			}
			if args.BeforeNumber < 0 || limit < 1 || limit > archiveHistoryLimit {
				return c.failure(errors.New("before_number must be nonnegative and limit must be 1–10")), nil
			}
			detail, err := c.service.Document(ctx, args.DocumentID, args.BeforeNumber, limit)
			if err != nil {
				return c.failure(archiveSafeError(err)), nil
			}
			card := archiveDetail(detail)
			c.addDocumentAndVersions(card)
			reply := archiveToolReply{OK: true, Document: &card.Document, Detail: &card}
			bounded, err := boundArchiveToolReply(ctx, reply)
			if err != nil {
				return c.failure(errors.New("archive history exceeds the response limit; request fewer versions")), nil
			}
			return bounded, nil
		}
		if !c.knowsVersion(args.VersionID) {
			return c.failure(errors.New("version_id must be returned by an archive search/detail tool or stated by the current user")), nil
		}
		version, err := c.service.Version(ctx, args.VersionID)
		if err != nil {
			return c.failure(archiveSafeError(err)), nil
		}
		c.knownDocuments[version.DocumentID] = true
		card := versionCard(clipArchiveVersion(version))
		return archiveToolReply{OK: true, Version: &card}, nil
	})
	if err != nil {
		return nil, err
	}

	download, err := utils.InferTool("get_archive_download_link", "按指定 version_id 生成短时原件下载链接。链接本身会随最终回复提供；不要复制、改写或在工具调用前宣称链接已就绪。工具只返回上传状态和过期时间。", func(ctx context.Context, args archiveVersionArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		if args.VersionID <= 0 || !c.knowsVersion(args.VersionID) {
			return c.failure(errors.New("version_id must be returned by an archive search/detail tool or stated by the current user")), nil
		}
		link, err := c.service.DownloadLink(ctx, args.VersionID)
		if err != nil {
			c.addMandatoryReceipt(fmt.Sprintf("download:%d", args.VersionID), "该版本的下载链接未能生成。")
			return c.failure(archiveSafeError(err)), nil
		}
		ready := link.URL != ""
		if ready {
			c.downloads = append(c.downloads, link)
		} else {
			c.addMandatoryReceipt(fmt.Sprintf("download:%d", args.VersionID), fmt.Sprintf("该版本原件暂不可下载，当前保存状态：%s。", archiveStatusText(link.UploadStatus)))
		}
		return archiveToolReply{OK: true, Download: &archiveDownloadCard{VersionID: link.VersionID, UploadState: link.UploadStatus, Ready: ready, ExpiresAt: link.ExpiresAt}}, nil
	})
	if err != nil {
		return nil, err
	}

	read, err := utils.InferTool("read_archive_text", "读取指定归档版本中已提取文字的一页。offset/limit 按 Unicode 码点计算；后续页传回 version。返回原件位置和提取范围；若没有可用文字，查看 summary_status/scope 并如实说明。", func(ctx context.Context, args archiveReadTextArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		if args.VersionID <= 0 || !c.knowsVersion(args.VersionID) {
			return c.failure(errors.New("version_id must be returned by an archive search/detail tool or stated by the current user")), nil
		}
		if _, err := args.Request.Validate(); err != nil {
			return c.failure(errors.New("invalid text page request")), nil
		}
		text, err := c.service.ReadText(ctx, args.VersionID, args.Request)
		if err != nil {
			return c.failure(archiveSafeError(err)), nil
		}
		reply := archiveToolReply{OK: true, Text: &text}
		bounded, err := boundArchiveToolReply(ctx, reply)
		if err != nil {
			return c.failure(errors.New("requested text page exceeds the response limit; request a smaller page")), nil
		}
		return bounded, nil
	})
	if err != nil {
		return nil, err
	}

	prepare, err := utils.InferTool("prepare_archive_attachment", "准备并返回指定归档版本的原件附件 ID，用于用户明确要求把文件发回聊天时。上传中的版本如果聊天原件仍可读，也可以直接交付。返回 ID 不等于已发送；只有用户确实要收到文件时，才在最终回复的 attachments 中选择它。", func(ctx context.Context, args archiveVersionArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		if args.VersionID <= 0 || !c.knowsVersion(args.VersionID) {
			return c.failure(errors.New("version_id must be returned by an archive search/detail tool or stated by the current user")), nil
		}
		original, err := c.service.PrepareAttachment(ctx, args.VersionID)
		if err != nil {
			c.addMandatoryReceipt(fmt.Sprintf("prepare:%d", args.VersionID), "该版本的原件暂时无法准备交付。")
			return c.failure(archiveSafeError(err)), nil
		}
		if original.AttachmentID != "" {
			file, err := c.files.Attachment(ctx, original.AttachmentID)
			if err != nil || file.Deleted || file.Error != "" {
				c.addMandatoryReceipt(fmt.Sprintf("prepare:%d", args.VersionID), "该版本的原件附件当前不可用。")
				return c.failure(errors.New("prepared original attachment is unavailable")), nil
			}
			if c.createdAttachment != nil {
				c.createdAttachment(file)
			}
			c.preparedAttachments = append(c.preparedAttachments, file)
		}
		if original.AttachmentID == "" {
			c.addMandatoryReceipt(fmt.Sprintf("prepare:%d", args.VersionID), fmt.Sprintf("该版本目前没有可直接交付的原件附件，原件保存状态：%s。", archiveStatusText(original.UploadStatus)))
		}
		return archiveToolReply{OK: true, Original: &original}, nil
	})
	if err != nil {
		return nil, err
	}
	tools := []tool.BaseTool{archive, search, get, download, read, prepare}
	edits, err := c.buildEdits()
	if err != nil {
		return nil, err
	}
	return append(tools, edits...), nil
}

func (c *archiveTurnTools) requireDirectUser() error {
	if c.service == nil || c.files == nil {
		return errors.New("archive service is not configured")
	}
	if c.event.Kind != personal.EventUser || c.event.ID <= 0 {
		return errors.New("archive tools require a persisted direct user event")
	}
	return nil
}

func (c *archiveTurnTools) failure(err error) archiveToolReply {
	return archiveToolReply{OK: false, Error: err.Error()}
}

func (c *archiveTurnTools) addMandatoryReceipt(key, text string) {
	if _, exists := c.mandatoryReceipts[key]; !exists {
		c.receiptOrder = append(c.receiptOrder, key)
	}
	c.mandatoryReceipts[key] = text
}

func (c *archiveTurnTools) validateMetadata(category string, tags []string) error {
	if utf8.RuneCountInString(strings.TrimSpace(category)) > 128 || len(tags) > 20 {
		return errors.New("category must be at most 128 characters and at most 20 tags may be supplied")
	}
	for _, tag := range tags {
		if utf8.RuneCountInString(strings.TrimSpace(tag)) > 64 {
			return errors.New("each tag must be at most 64 characters")
		}
	}
	return nil
}

func (c *archiveTurnTools) knowsDocument(id int64) bool {
	if c.knownDocuments[id] {
		return true
	}
	return id > 0 && mentionsArchiveID(c.event.Text, id)
}

func (c *archiveTurnTools) knowsVersion(id int64) bool {
	if c.knownVersions[id] {
		return true
	}
	return id > 0 && mentionsArchiveID(c.event.Text, id)
}

func mentionsArchiveID(text string, id int64) bool {
	pattern := regexp.MustCompile(fmt.Sprintf(archiveNumericID.String(), id))
	return pattern.MatchString(text)
}

func (c *archiveTurnTools) addDocumentAndVersions(detail archiveDetailCard) {
	c.knownDocuments[detail.Document.ID] = true
	c.knownVersions[detail.Latest.ID] = true
	for _, version := range detail.Versions {
		c.knownVersions[version.ID] = true
	}
}

func (c *archiveTurnTools) finalizeReply(response personalTurnResponse) personalTurnResponse {
	if len(c.receiptOrder) > 0 {
		for _, key := range c.receiptOrder {
			response.Text = appendReplyText(response.Text, c.mandatoryReceipts[key])
		}
		response.ReplyKind = personal.ReplyContent
	}
	for _, link := range c.downloads {
		response.Text = appendReplyText(response.Text, fmt.Sprintf("版本 %d 的下载链接（有效期至 %s）：\n%s", link.VersionID, link.ExpiresAt.UTC().Format(time.RFC3339), link.URL))
		response.ReplyKind = personal.ReplyContent
		response.SkipReplyPresentation = true
	}
	if wantsArchiveAttachmentDelivery(c.event.Text) {
		seen := make(map[string]bool, len(response.Attachments))
		for _, id := range response.Attachments {
			seen[id] = true
		}
		for _, file := range c.preparedAttachments {
			if !seen[file.ID] {
				response.Attachments = append(response.Attachments, file.ID)
				seen[file.ID] = true
			}
		}
		if len(response.Attachments) > 0 {
			response.ReplyKind = personal.ReplyContent
		}
	}
	return response
}

func (c *archiveTurnTools) hasMandatoryReply() bool {
	return len(c.receiptOrder) > 0 || len(c.downloads) > 0
}

func wantsArchiveAttachmentDelivery(text string) bool {
	text = strings.ToLower(text)
	for _, phrase := range []string{"发我原件", "发我文件", "发给我原件", "发给我文件", "把文件发我", "把原件发我", "把附件发我", "发回来", "发回聊天", "发送原件", "下载原件并发送", "download the original", "send me the file", "send me the original", "send the file", "attach the original", "return the file"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func appendReplyText(text, addition string) string {
	if strings.TrimSpace(text) == "" {
		return addition
	}
	return strings.TrimSpace(text) + "\n\n" + addition
}

func archiveReceipt(version documentarchive.Version) string {
	state := "已登记"
	if version.Archived() {
		state = "已归档"
	} else {
		state += fmt.Sprintf("（原件：%s，简述：%s）", archiveStatusText(version.UploadStatus), archiveStatusText(version.SummaryStatus))
	}
	return fmt.Sprintf("%s：文档 %d 的第 %d 版（版本记录 %d）。", state, version.DocumentID, version.Number, version.ID)
}

func archiveStatusText(status documentarchive.Status) string {
	switch status {
	case documentarchive.StatusReady:
		return "就绪"
	case documentarchive.StatusPending:
		return "待处理"
	case documentarchive.StatusProcessing:
		return "处理中"
	case documentarchive.StatusFailed:
		return "失败"
	case documentarchive.StatusNeedsNote:
		return "待补充说明"
	default:
		return string(status)
	}
}

func archiveSafeError(err error) error {
	switch {
	case errors.Is(err, documentarchive.ErrNotFound):
		return errors.New("document or version was not found")
	case errors.Is(err, documentarchive.ErrConflict):
		return errors.New("version conflicts with an existing archive record")
	default:
		return errors.New("archive operation failed")
	}
}

func normalizedArchiveTags(tags []string) []string {
	result := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag != "" && !seen[strings.ToLower(tag)] {
			seen[strings.ToLower(tag)] = true
			result = append(result, tag)
		}
	}
	return result
}

func archiveDetail(detail documentarchive.Detail) archiveDetailCard {
	detail.Document = clipArchiveDocument(detail.Document)
	versions := make([]archiveVersionCard, 0, len(detail.Versions))
	for _, version := range detail.Versions {
		versions = append(versions, versionCard(clipArchiveVersion(version)))
	}
	return archiveDetailCard{
		Document: detail.Document, Latest: versionCard(clipArchiveVersion(detail.Latest)), Versions: versions,
		NextBeforeNumber: detail.NextBeforeNumber,
	}
}

func clipArchiveDocument(document documentarchive.Document) documentarchive.Document {
	document.Title = clipArchiveText(document.Title, 200)
	document.Category = clipArchiveText(document.Category, 128)
	if len(document.Tags) > 20 {
		document.Tags = document.Tags[:20]
	}
	for i := range document.Tags {
		document.Tags[i] = clipArchiveText(document.Tags[i], 64)
	}
	return document
}

func clipArchiveVersion(version documentarchive.Version) documentarchive.Version {
	version.Name = clipArchiveText(version.Name, 200)
	version.Note = clipArchiveText(version.Note, 500)
	version.Summary = clipArchiveText(version.Summary, 800)
	version.SummaryScope = clipArchiveText(version.SummaryScope, 300)
	return version
}

func clipArchiveText(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}

func boundArchiveToolReply(ctx context.Context, reply archiveToolReply) (archiveToolReply, error) {
	_, err := textpage.EncodeBounded(&reply, textpage.MaxBytes(ctx), func() bool {
		if reply.Text != nil && reply.Text.Page != nil && reply.Text.Page.Shrink() {
			return true
		}
		if reply.Detail != nil {
			if len(reply.Detail.Versions) > 1 {
				reply.Detail.Versions = reply.Detail.Versions[:len(reply.Detail.Versions)-1]
				reply.Detail.NextBeforeNumber = reply.Detail.Versions[len(reply.Detail.Versions)-1].Number
				return true
			}
			if shrinkArchiveVersion(&reply.Detail.Latest) {
				return true
			}
			if len(reply.Detail.Versions) == 1 && shrinkArchiveVersion(&reply.Detail.Versions[0]) {
				return true
			}
			if shrinkArchiveDocument(&reply.Detail.Document) {
				return true
			}
		}
		if reply.Document != nil && shrinkArchiveDocument(reply.Document) {
			return true
		}
		if len(reply.Hits) > 1 {
			reply.Hits = reply.Hits[:len(reply.Hits)-1]
			reply.HasMore = true
			reply.NextOffset = reply.Offset + len(reply.Hits)
			return true
		}
		if len(reply.Hits) == 1 {
			hit := &reply.Hits[0]
			if shrinkArchiveVersion(&hit.Match) || shrinkArchiveVersion(&hit.Latest) || shrinkArchiveDocument(&hit.Document) {
				return true
			}
		}
		return false
	})
	return reply, err
}

func shrinkArchiveVersion(version *archiveVersionCard) bool {
	for _, field := range []*string{&version.Name, &version.Note, &version.Summary, &version.SummaryScope} {
		if shrinkArchiveString(field) {
			return true
		}
	}
	return false
}

func shrinkArchiveDocument(document *documentarchive.Document) bool {
	if shrinkArchiveString(&document.Title) || shrinkArchiveString(&document.Category) {
		return true
	}
	if len(document.Tags) > 0 {
		document.Tags = document.Tags[:len(document.Tags)-1]
		return true
	}
	return false
}

func shrinkArchiveString(value *string) bool {
	runes := []rune(*value)
	if len(runes) == 0 {
		return false
	}
	keep := len(runes) / 2
	if keep == 0 {
		keep = 0
	}
	if keep == len(runes) {
		return false
	}
	*value = string(runes[:keep])
	return true
}
