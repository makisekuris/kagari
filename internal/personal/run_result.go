package personal

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"kagari/internal/channel"
	"kagari/internal/domain"
	"kagari/internal/personal/internal/textclip"
)

const maxTaskRunes = 2_000
const resultOmissionHint = "[Some result details were omitted; use read_run with this run ID and section=result for the full result.]"

type RunResultCard struct {
	Summary             string             `json:"summary,omitempty"`
	Sources             []domain.SourceRef `json:"sources,omitempty"`
	MaterialEvidenceIDs []string           `json:"material_evidence_ids,omitempty"`
	Files               []string           `json:"files,omitempty"`
	AttachmentIDs       []string           `json:"attachment_ids,omitempty"`
	ReadRun             string             `json:"read_run"`
}

func SummarizeRunResult(run Run) RunResultCard {
	card := RunResultCard{}
	if run.ID > 0 {
		card.ReadRun = fmt.Sprintf("read_run run_id=%d section=result", run.ID)
	}
	if strings.TrimSpace(run.Result) == "" {
		return card
	}
	var result StageResult
	if json.Unmarshal([]byte(run.Result), &result) == nil && strings.HasPrefix(strings.TrimSpace(run.Result), "{") {
		card.Summary = result.Summary
		card.Sources = append([]domain.SourceRef(nil), result.Sources...)
		for _, progress := range result.Progress {
			for _, evidence := range progress.Evidence {
				if evidence.Kind == "material" {
					card.MaterialEvidenceIDs = append(card.MaterialEvidenceIDs, evidence.ID)
				}
			}
		}
		card.Files = append([]string(nil), result.Files...)
		for _, attachment := range result.Attachments {
			card.AttachmentIDs = append(card.AttachmentIDs, attachment.ID)
		}
	} else {
		card.Summary = run.Result
	}
	seenMaterials := make(map[string]struct{}, len(card.MaterialEvidenceIDs))
	for _, id := range MaterialReferences(card.Summary) {
		card.MaterialEvidenceIDs = append(card.MaterialEvidenceIDs, id)
	}
	materialIDs := card.MaterialEvidenceIDs
	card.MaterialEvidenceIDs = nil
	for _, id := range materialIDs {
		if _, exists := seenMaterials[id]; exists {
			continue
		}
		seenMaterials[id] = struct{}{}
		card.MaterialEvidenceIDs = append(card.MaterialEvidenceIDs, id)
	}
	for _, id := range card.MaterialEvidenceIDs {
		card.Summary = strings.ReplaceAll(card.Summary, id, "")
	}
	seenAttachments := make(map[string]struct{}, len(card.AttachmentIDs))
	attachments := card.AttachmentIDs
	card.AttachmentIDs = nil
	for _, id := range attachments {
		if _, exists := seenAttachments[id]; exists {
			continue
		}
		seenAttachments[id] = struct{}{}
		card.AttachmentIDs = append(card.AttachmentIDs, id)
	}
	return card
}

func boundedTaskResult(raw string) string {
	var result StageResult
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") || json.Unmarshal([]byte(raw), &result) != nil {
		return textclip.Limit(raw, maxTaskRunes)
	}
	if utf8.RuneCountInString(raw) <= maxTaskRunes {
		return raw
	}

	hint := ""
	if len(result.Progress) > 0 || len(result.AppliedInputIDs) > 0 {
		result.Progress = nil
		result.AppliedInputIDs = nil
		hint = resultOmissionHint
	}
	files := append([]string(nil), result.Files...)
	for i := range result.Sources {
		result.Sources[i].Title = textclip.Limit(result.Sources[i].Title, 200)
	}
	for i := range result.Attachments {
		attachment := &result.Attachments[i]
		attachment.Name = textclip.Limit(attachment.Name, 200)
		attachment.MIME = textclip.Limit(attachment.MIME, 128)
		attachment.Error = textclip.Limit(attachment.Error, 256)
	}
	attachments := append([]channel.Attachment(nil), result.Attachments...)
	summary := result.Summary
	for {
		result.Summary = summary
		if hint != "" {
			if result.Summary != "" {
				result.Summary += "\n"
			}
			result.Summary += hint
		}
		encoded, _ := json.Marshal(result)
		if utf8.RuneCountInString(string(encoded)) <= maxTaskRunes {
			return string(encoded)
		}
		if summary != "" {
			summary = textclip.Limit(summary, utf8.RuneCountInString(summary)/2)
		} else if len(result.Sources) > 0 {
			result.Sources = result.Sources[:len(result.Sources)-1]
		} else {
			break
		}
	}

	result.Summary = resultOmissionHint
	result.Files = nil
	result.Attachments = nil
	filesFit, attachmentsFit := true, true
	for i := 0; i < len(files) || i < len(attachments); i++ {
		if i < len(files) && filesFit {
			result.Files = append(result.Files, files[i])
			encoded, _ := json.Marshal(result)
			if utf8.RuneCountInString(string(encoded)) > maxTaskRunes {
				result.Files = result.Files[:len(result.Files)-1]
				filesFit = false
			}
		}
		if i < len(attachments) && attachmentsFit {
			result.Attachments = append(result.Attachments, attachments[i])
			encoded, _ := json.Marshal(result)
			if utf8.RuneCountInString(string(encoded)) > maxTaskRunes {
				result.Attachments = result.Attachments[:len(result.Attachments)-1]
				attachmentsFit = false
			}
		}
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}
