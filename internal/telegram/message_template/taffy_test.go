package messagetemplate_test

import (
	"fmt"
	messagetemplate "kagari/internal/telegram/message_template"
	"strings"
	"testing"
)

func TestTaffyAsk(test *testing.T) {
	taffy := messagetemplate.Taffy{}
	seen := map[string]bool{}
	for jobID := int64(1); jobID <= 10; jobID++ {
		output := taffy.AskChatID(jobID)
		if !strings.Contains(output, fmt.Sprintf("#%d", jobID)) {
			test.Fatalf("output %q missing job ID %d", output, jobID)
		}
		if seen[output] {
			test.Fatalf("duplicate acknowledgement: %q", output)
		}
		seen[output] = true
	}
}
