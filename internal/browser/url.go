package browser

import (
	"net/url"
	"strings"

	"kagari/internal/reader"
)

// NormalizeURL validates and canonicalizes a browser URL while keeping its fragment.
func NormalizeURL(raw string) (string, error) {
	normalized, err := reader.NormalizeURL(raw)
	if err != nil {
		return "", err
	}
	original, _ := url.Parse(strings.TrimSpace(raw))
	canonical, _ := url.Parse(normalized)
	canonical.Fragment, canonical.RawFragment = original.Fragment, original.RawFragment
	return canonical.String(), nil
}
