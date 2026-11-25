package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/documentarchive"
)

type archiveDocumentEditArgs struct {
	DocumentID int64     `json:"document_id"`
	Title      *string   `json:"title,omitempty"`
	Category   *string   `json:"category,omitempty"`
	Tags       *[]string `json:"tags,omitempty"`
}

type archiveSummaryEditArgs struct {
	VersionID int64  `json:"version_id"`
	Summary   string `json:"summary"`
}

func (c *archiveTurnTools) buildEdits() ([]tool.BaseTool, error) {
	document, err := utils.InferTool("update_archive_document", "根据使用者明确要求修改归档文档的标题、分类或标签。只传要变更的字段。空分类清除分类，空标签列表清除标签。更新文档资料后，原有版本可以照常查询。", func(ctx context.Context, args archiveDocumentEditArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		if !c.knowsDocument(args.DocumentID) {
			return c.failure(errors.New("先查询目标文档或使用用户明确指定的 document_id")), nil
		}
		doc, err := c.service.UpdateDocument(ctx, documentarchive.DocumentPatch{
			EventID: c.event.ID, DocumentID: args.DocumentID, Title: args.Title, Category: args.Category, Tags: args.Tags,
		})
		if err != nil {
			return c.failure(archiveSafeError(err)), nil
		}
		c.addMandatoryReceipt(fmt.Sprintf("edit-document-%d", doc.ID), fmt.Sprintf("已更新「%s」的归档资料。", doc.Title))
		return archiveToolReply{OK: true, Document: &doc}, nil
	})
	if err != nil {
		return nil, err
	}
	summary, err := utils.InferTool("update_archive_summary", "根据用户本次明确提供的说明，补充或纠正指定版本的归档简述。summary 逐字取自本条用户消息，最多 240 个字符。也用于补全状态为待补充说明的版本。", func(ctx context.Context, args archiveSummaryEditArgs) (archiveToolReply, error) {
		if err := c.requireDirectUser(); err != nil {
			return c.failure(err), nil
		}
		if !c.knowsVersion(args.VersionID) {
			return c.failure(errors.New("先查询版本或使用用户明确指定的 version_id")), nil
		}
		note := strings.TrimSpace(args.Summary)
		if note == "" || !strings.Contains(c.event.Text, note) {
			return c.failure(errors.New("简述必须来自本条用户消息中的说明")), nil
		}
		version, err := c.service.UpdateSummary(ctx, c.event.ID, args.VersionID, note)
		if errors.Is(err, documentarchive.ErrSummaryPending) {
			return c.failure(errors.New("简述正在生成，可以在处理结束后修改")), nil
		}
		if err != nil {
			return c.failure(archiveSafeError(err)), nil
		}
		c.addMandatoryReceipt(fmt.Sprintf("edit-summary-%d", version.ID), fmt.Sprintf("已更新第 %d 版的简述。", version.Number))
		card := versionCard(version)
		return archiveToolReply{OK: true, Version: &card}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{document, summary}, nil
}
