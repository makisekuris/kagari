package assistant

import (
	"kagari/internal/channel"
	"kagari/internal/personal"
)

// Include recent messages so a question can refer to a file sent in the previous turn.
func turnImageAttachments(input personal.TurnInput) map[string]bool {
	allowed := map[string]bool{}
	add := func(files []channel.Attachment) {
		for _, file := range files {
			if file.ID != "" && !file.Deleted && file.Error == "" {
				allowed[file.ID] = true
			}
		}
	}
	add(input.Event.Attachments)
	if input.RepliedMessage != nil {
		add(input.RepliedMessage.Attachments)
	}
	for _, message := range input.History {
		add(message.Attachments)
	}
	return allowed
}
