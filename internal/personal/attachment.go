package personal

import (
	"path/filepath"
	"strings"
)

func SafeAttachmentName(name string) string {
	name = strings.ReplaceAll(name, `\\`, "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name))
	if name == "." || name == ".." {
		return ""
	}
	return name
}
