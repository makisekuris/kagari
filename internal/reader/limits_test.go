package reader

import (
	"strings"
	"testing"
)

func TestArticleCanDiscussBrowserChallenges(t *testing.T) {
	if restrictedPage([]byte("<html><p>A tutorial about CAPTCHA widgets</p></html>")) {
		t.Fatal("ordinary topic was treated as an access gate")
	}
	article := "<html><article>" + strings.Repeat("An engineering explanation. ", 100) + "Verify you are human</article></html>"
	if restrictedPage([]byte(article)) {
		t.Fatal("a phrase inside a full article was treated as a gate")
	}
	if !restrictedPage([]byte("<html><h1>Verify you are human</h1><script>challenge-platform</script></html>")) {
		t.Fatal("short gate page not detected")
	}
	if len([]rune(excerpt(strings.Repeat("文", 1000), 400))) != 401 {
		t.Fatal("link context not bounded")
	}
}
