package personal

import (
	"encoding/json"
	"strings"

	"kagari/internal/channel"
)

func boundedTaskResult(raw string) string {
	var result StageResult
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") || json.Unmarshal([]byte(raw), &result) != nil {
		return limitRunes(raw, maxTaskRunes)
	}
	if runeLen(raw) <= maxTaskRunes {
		return raw
	}

	files := append([]string(nil), result.Files...)
	for i := range result.Sources {
		source := &result.Sources[i]
		source.Content, source.Links = "", nil
		source.ID = limitRunes(source.ID, 128)
		source.RequestedURL, source.URL = limitRunes(source.RequestedURL, 512), limitRunes(source.URL, 512)
		source.Kind, source.Status = limitRunes(source.Kind, 64), limitRunes(source.Status, 32)
		source.Title, source.Author = limitRunes(source.Title, 200), limitRunes(source.Author, 128)
		source.Reason, source.ReadMethod = limitRunes(source.Reason, 256), limitRunes(source.ReadMethod, 64)
	}
	for i := range result.Attachments {
		attachment := &result.Attachments[i]
		attachment.Name = limitRunes(attachment.Name, 200)
		attachment.MIME = limitRunes(attachment.MIME, 128)
		attachment.Error = limitRunes(attachment.Error, 256)
	}
	attachments := append([]channel.Attachment(nil), result.Attachments...)
	for {
		encoded, _ := json.Marshal(result)
		if runeLen(string(encoded)) <= maxTaskRunes {
			return string(encoded)
		}
		if result.Summary != "" {
			result.Summary = limitRunes(result.Summary, runeLen(result.Summary)/2)
		} else if len(result.Sources) > 0 {
			result.Sources = result.Sources[:len(result.Sources)-1]
		} else {
			break
		}
	}

	result.Summary = "[Some file references were omitted; use get_run for the full result.]"
	result.Files = nil
	result.Attachments = nil
	filesFit, attachmentsFit := true, true
	for i := 0; i < len(files) || i < len(attachments); i++ {
		if i < len(files) && filesFit {
			result.Files = append(result.Files, files[i])
			encoded, _ := json.Marshal(result)
			if runeLen(string(encoded)) > maxTaskRunes {
				result.Files = result.Files[:len(result.Files)-1]
				filesFit = false
			}
		}
		if i < len(attachments) && attachmentsFit {
			result.Attachments = append(result.Attachments, attachments[i])
			encoded, _ := json.Marshal(result)
			if runeLen(string(encoded)) > maxTaskRunes {
				result.Attachments = result.Attachments[:len(result.Attachments)-1]
				attachmentsFit = false
			}
		}
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}
