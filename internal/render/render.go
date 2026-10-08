package render

import (
	"strings"

	"kagari/internal/domain"
)

func Analysis(result domain.Result) string { return strings.TrimSpace(result.Body) }
