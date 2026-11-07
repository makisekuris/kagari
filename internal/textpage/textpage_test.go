package textpage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadUsesUnicodeRuneOffsetsAndAcceptsNUL(t *testing.T) {
	body := "猫\x00🧪Z"
	page, err := Read(body, Request{Limit: 3}, false)
	if err != nil {
		t.Fatal(err)
	}
	if page.Text != "猫\x00🧪" || page.Offset != 0 || page.End != 3 || page.TotalRunes != 4 || !page.HasMore || page.NextOffset == nil || *page.NextOffset != 3 {
		t.Fatalf("first page = %+v", page)
	}
	last, err := Read(body, Request{Offset: 3, Version: page.Version}, false)
	if err != nil || last.Text != "Z" || last.End != 4 || last.HasMore {
		t.Fatalf("last page = %+v, err %v", last, err)
	}
	empty, err := Read(body, Request{Offset: 4}, false)
	if err != nil || empty.Text != "" || empty.End != 4 || empty.HasMore {
		t.Fatalf("empty end page = %+v, err %v", empty, err)
	}
}

func TestReadValidatesPagesAndVersions(t *testing.T) {
	body := "original"
	for _, req := range []Request{{Offset: -1}, {Limit: -1}, {Limit: MaxTextLimit + 1}, {Offset: len(body) + 1}} {
		if _, err := Read(body, req, false); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("Read(%+v) error = %v", req, err)
		}
	}
	if _, err := Read(body, Request{Offset: 1}, true); !errors.Is(err, ErrVersionRequired) {
		t.Fatalf("mutable continuation error = %v", err)
	}
	_, err := Read("updated", Request{Offset: 1, Version: Version(body)}, true)
	var changed *VersionChangedError
	if !errors.As(err, &changed) || changed.Version != Version("updated") {
		t.Fatalf("version change error = %#v", err)
	}
	if Version("same") != Version("same") || Version("same") == Version("different") {
		t.Fatal("content version is not stable and content-addressed")
	}
}

func TestRequestDefaultsAndListBounds(t *testing.T) {
	if got, err := (Request{}).Validate(); err != nil || got != DefaultTextLimit {
		t.Fatalf("default text limit = %d, %v", got, err)
	}
	if got, err := ValidateList("", 0); err != nil || got != DefaultListLimit {
		t.Fatalf("default list limit = %d, %v", got, err)
	}
	if got, err := ValidateList("item", MaxListLimit); err != nil || got != MaxListLimit {
		t.Fatalf("max list limit = %d, %v", got, err)
	}
	for _, limit := range []int{-1, MaxListLimit + 1} {
		if _, err := ValidateList("", limit); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ValidateList limit %d error = %v", limit, err)
		}
	}
}

func TestResponseBudgetDefaultsAndClamps(t *testing.T) {
	ctx := context.Background()
	if got := MaxBytes(ctx); got != MaxJSONBytes {
		t.Fatalf("default response limit = %d", got)
	}
	if got := MaxBytes(WithMaxBytes(ctx, 8192)); got != 8192 {
		t.Fatalf("response limit = %d", got)
	}
	if got := MaxBytes(WithMaxBytes(ctx, MaxJSONBytes+1)); got != MaxJSONBytes {
		t.Fatalf("clamped response limit = %d", got)
	}
	if got := MaxBytes(WithMaxBytes(ctx, 0)); got != 1 {
		t.Fatalf("minimum response limit = %d", got)
	}
}

func TestEncodeBoundedShrinksEscapedTextAndKeepsOffsetsValid(t *testing.T) {
	page, err := Read(strings.Repeat("<", MaxTextLimit), Request{Limit: MaxTextLimit}, false)
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		Page Page `json:"page"`
	}
	reply := response{Page: page}
	encoded, err := EncodeBounded(&reply, MaxJSONBytes, reply.Page.Shrink)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxJSONBytes {
		t.Fatalf("encoded response is %d bytes", len(encoded))
	}
	var decoded response
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Page.Text == "" || decoded.Page.Text == strings.Repeat("<", MaxTextLimit) || !decoded.Page.HasMore || decoded.Page.End != utf8.RuneCountInString(decoded.Page.Text) || decoded.Page.NextOffset == nil || *decoded.Page.NextOffset != decoded.Page.End {
		t.Fatalf("shrunk page has inconsistent offsets: %+v", decoded.Page)
	}
	if _, err := EncodeBounded(strings.Repeat("x", MaxJSONBytes), MaxJSONBytes, nil); !errors.Is(err, ErrResponseItemTooLarge) {
		t.Fatalf("oversized required response error = %v", err)
	}
}
