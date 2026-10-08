package render

import (
	"testing"

	"kagari/internal/domain"
)

func TestAnalysisPreservesMarkdown(t *testing.T) {
	body := "## 我的判断\n\n正文 [原文](https://example.org)"
	if Analysis(domain.Result{Body: body}) != body {
		t.Fatal("model prose was altered")
	}
}
