package personal

import (
	"fmt"
	"testing"
)

func TestIsPureURLMessageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"https://example.com/article", true},
		{"https://example.com/a\nhttp://example.org/b?q=1", true},
		{"  http://example.com  https://example.org  ", true},
		{"", false},
		{"   \n\t", false},
		{"Read this https://example.com/article", false},
		{"/cancel_task https://example.com/article", false},
		{"ftp://example.com/file", false},
		{"http:/example.com", false},
		{"https://", false},
		{"https://example.com note", false},
	} {
		t.Run(fmt.Sprintf("%q", tc.text), func(t *testing.T) {
			if got := IsPureURLMessage(tc.text); got != tc.want {
				t.Fatalf("IsPureURLMessage(%q)=%v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
