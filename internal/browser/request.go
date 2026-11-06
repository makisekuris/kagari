package browser

import (
	"errors"
	"regexp"
	"strings"
)

// Request is one bounded action in the isolated browser session.
type Request struct {
	Action  string `json:"action,omitempty" jsonschema:"description=Action to perform. Empty defaults to navigate.,enum=navigate,enum=snapshot,enum=click,enum=type,enum=press_key,enum=wait_for,enum=back"`
	URL     string `json:"url,omitempty" jsonschema:"description=HTTP(S) URL for navigate only"`
	Ref     string `json:"ref,omitempty" jsonschema:"description=Element ref from the latest snapshot for click or type"`
	Text    string `json:"text,omitempty" jsonschema:"description=Text to type (empty clears a field) or text to wait for"`
	Key     string `json:"key,omitempty" jsonschema:"description=Keyboard key for press_key"`
	Seconds int    `json:"seconds,omitempty" jsonschema:"description=Wait duration in seconds from 1 to 30; zero is allowed with Text"`
}

var snapshotRefPattern = regexp.MustCompile(`^(?:f\d+)?e\d+$`)
var snapshotRefCapture = regexp.MustCompile(`\[ref=((?:f\d+)?e\d+)\]`)

// Normalized validates the action-specific fields before any browser side effect.
func (r Request) Normalized() (Request, error) {
	r.Action = strings.ToLower(strings.TrimSpace(r.Action))
	if r.Action == "" {
		r.Action = "navigate"
	}
	r.URL = strings.TrimSpace(r.URL)
	r.Ref = strings.TrimSpace(r.Ref)
	r.Key = strings.TrimSpace(r.Key)
	fields := func(url, ref, text, key, seconds bool) bool {
		return (url || r.URL == "") && (ref || r.Ref == "") && (text || r.Text == "") && (key || r.Key == "") && (seconds || r.Seconds == 0)
	}
	switch r.Action {
	case "navigate":
		if !fields(true, false, false, false, false) {
			return Request{}, errors.New("navigate accepts only URL")
		}
		u, err := NormalizeURL(r.URL)
		if err != nil {
			return Request{}, errors.New("navigate requires a valid HTTP(S) URL")
		}
		r.URL = u
	case "snapshot", "back":
		if !fields(false, false, false, false, false) {
			return Request{}, errors.New(r.Action + " accepts no other fields")
		}
	case "click":
		if r.Ref == "" || !snapshotRefPattern.MatchString(r.Ref) || !fields(false, true, false, false, false) {
			return Request{}, errors.New("click requires only a snapshot Ref such as e12")
		}
	case "type":
		if r.Ref == "" || !snapshotRefPattern.MatchString(r.Ref) || !fields(false, true, true, false, false) {
			return Request{}, errors.New("type requires a snapshot Ref such as e12")
		}
	case "press_key":
		if r.Key == "" || !fields(false, false, false, true, false) {
			return Request{}, errors.New("press_key requires only Key")
		}
	case "wait_for":
		if !fields(false, false, true, false, true) || r.Seconds < 0 || r.Seconds > 30 || (strings.TrimSpace(r.Text) == "" && r.Seconds == 0) {
			return Request{}, errors.New("wait_for requires Text or Seconds from 1 to 30")
		}
	default:
		return Request{}, errors.New("unsupported browser action")
	}
	return r, nil
}
