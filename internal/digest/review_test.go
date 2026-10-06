package digest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestPrepareFiltersDeduplicatesAndProjects(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	at := start.Add(time.Hour)
	makeEntry := func(id int64, user int64, received time.Time, key string) domain.ArchiveEntry {
		return domain.ArchiveEntry{JobID: id, Submission: domain.Submission{
			UserID: user, ChatID: 700 + id, Text: "submission secret", ForwardedText: "forwarded secret",
			URLs: []string{"https://submitted.example"}, CacheKey: key, ReceivedAt: received,
		}, Result: domain.Result{Analysis: domain.Analysis{Title: "title"}}}
	}
	first := makeEntry(9, 7, at, "Exact")
	first.Result.Analysis = domain.Analysis{
		Headings: &domain.AnalysisHeadings{Summary: "heading"}, Title: "title", Overview: "overview",
		Summary: []domain.Claim{{Text: "claim", SourceIDs: []string{"source-1"}}},
		Tags:    []string{"tag"},
	}
	first.Result.Sources = []domain.Source{{ID: "source-1", Title: "source", RequestedURL: "https://requested.example", URL: "https://final.example", Status: "ok", Content: "raw source body"}}
	entries := []domain.ArchiveEntry{
		first,
		makeEntry(2, 7, at.Add(time.Hour), "exact"),
		makeEntry(3, 7, at.Add(time.Hour), "Exact"), // exact key duplicate of job 9
		makeEntry(4, 7, at.Add(time.Hour), "EXACT"),
		makeEntry(5, 7, at.Add(time.Hour), ""),
		makeEntry(6, 7, at.Add(time.Hour), ""),
		makeEntry(7, 8, at, "other-user"),
		makeEntry(8, 7, start, "lower-bound"),
		makeEntry(10, 7, end, "upper-bound"),
	}
	request := domain.DigestRequest{UserID: 7, Start: start, End: end, Version: domain.DigestVersion}
	asOf := end.Add(time.Hour)
	input, err := Prepare(request, asOf, "UTC", "profile", "instruction", entries)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []int64{8, 9, 2, 4, 5, 6}
	if len(input.Entries) != len(wantIDs) {
		t.Fatalf("Prepare() selected %d entries, want %d: %+v", len(input.Entries), len(wantIDs), input.Entries)
	}
	for i, id := range wantIDs {
		if input.Entries[i].JobID != id {
			t.Fatalf("Prepare() entry %d has job id %d, want %d", i, input.Entries[i].JobID, id)
		}
	}
	if input.UserID != 7 || !input.Start.Equal(start) || !input.Cutoff.Equal(end) || !input.AsOf.Equal(asOf) || input.Profile != "profile" || input.Instruction != "instruction" {
		t.Fatalf("Prepare() lost request context: %+v", input)
	}
	if got := input.Entries[1]; got.Analysis.Headings == nil || got.Analysis.Headings.Summary != "heading" || got.Analysis.Summary[0].SourceIDs[0] != "source-1" || !got.Sources[0].Usable {
		t.Fatalf("Prepare() did not project complete analysis and safe source metadata: %+v", got)
	}
	entries[0].Result.Analysis.Headings.Summary = "mutated"
	entries[0].Result.Analysis.Summary[0].SourceIDs[0] = "mutated"
	entries[0].Result.Sources[0].Title = "mutated"
	if got := input.Entries[1]; got.Analysis.Headings.Summary != "heading" || got.Analysis.Summary[0].SourceIDs[0] != "source-1" || got.Sources[0].Title != "source" {
		t.Fatalf("Prepare() retained aliases to caller data: %+v", got)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"submission secret", "forwarded secret", "raw source body", "700", "submitted.example"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("prepared model input leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), "overview") || !strings.Contains(string(encoded), "source-1") {
		t.Fatalf("prepared model input omitted analysis: %s", encoded)
	}
}

func TestPrepareRejectsInvalidWindow(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		request domain.DigestRequest
		zone    string
	}{
		{name: "empty interval", request: domain.DigestRequest{Start: base, End: base}, zone: "UTC"},
		{name: "reversed interval", request: domain.DigestRequest{Start: base.Add(time.Hour), End: base}, zone: "UTC"},
		{name: "invalid timezone", request: domain.DigestRequest{Start: base, End: base.Add(time.Hour)}, zone: "No/Such_Zone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Prepare(tc.request, base, tc.zone, "", "", nil); err == nil {
				t.Fatal("Prepare() error = nil")
			}
		})
	}
}

func TestPrepareRequiresCurrentVersion(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	request := domain.DigestRequest{UserID: 7, Start: base, End: base.Add(time.Hour)}
	if _, err := Prepare(request, base, "UTC", "", "instruction", nil); err == nil {
		t.Fatal("Prepare() accepted a versionless request")
	}
}

func TestValidateReviewPartitionsMergedInputsAndEvidence(t *testing.T) {
	input := reviewInput()
	valid := domain.DigestReview{
		Opening: "Opening", Closing: "Closing",
		Sections: []domain.DigestSection{
			{Name: "Merged", Items: []domain.DigestItem{{EntryIDs: []int64{1, 2}, Title: "Combined", Review: "Review", Refs: []domain.DigestRef{{JobID: 1, SourceID: "a"}, {JobID: 2, SourceID: "b"}}}}},
			{Name: "No evidence", Items: []domain.DigestItem{{EntryIDs: []int64{3}, Title: "Legacy", Review: "Review"}}},
		},
	}
	if err := ValidateReview(input, valid); err != nil {
		t.Fatalf("ValidateReview(valid) = %v", err)
	}
	optionalText := cloneReview(valid)
	optionalText.Opening, optionalText.Closing = "", ""
	if err := ValidateReview(input, optionalText); err != nil {
		t.Fatalf("ValidateReview() rejected optional opening/closing: %v", err)
	}
	if Count(valid) != 2 {
		t.Fatalf("Count() = %d, want two merged review items", Count(valid))
	}

	cases := []struct {
		name string
		edit func(*domain.DigestReview)
	}{
		{name: "missing entry", edit: func(r *domain.DigestReview) { r.Sections[0].Items[0].EntryIDs = []int64{1} }},
		{name: "unknown entry", edit: func(r *domain.DigestReview) { r.Sections[0].Items[0].EntryIDs = []int64{1, 2, 99} }},
		{name: "duplicate entry", edit: func(r *domain.DigestReview) { r.Sections[0].Items[0].EntryIDs = []int64{1, 1, 2} }},
		{name: "cross item reference", edit: func(r *domain.DigestReview) {
			r.Sections[1].Items[0].Refs = []domain.DigestRef{{JobID: 1, SourceID: "a"}}
		}},
		{name: "duplicate reference", edit: func(r *domain.DigestReview) {
			r.Sections[0].Items[0].Refs = append(r.Sections[0].Items[0].Refs, domain.DigestRef{JobID: 1, SourceID: "a"})
		}},
		{name: "empty source id", edit: func(r *domain.DigestReview) {
			r.Sections[0].Items[0].Refs = []domain.DigestRef{{JobID: 1}}
		}},
		{name: "unusable reference", edit: func(r *domain.DigestReview) {
			r.Sections[1].Items[0].Refs = []domain.DigestRef{{JobID: 3, SourceID: "failed"}}
		}},
		{name: "usable entry omitted from refs", edit: func(r *domain.DigestReview) {
			r.Sections[0].Items[0].Refs = []domain.DigestRef{{JobID: 1, SourceID: "a"}}
		}},
		{name: "empty group", edit: func(r *domain.DigestReview) { r.Sections = append(r.Sections, domain.DigestSection{Name: "Empty"}) }},
		{name: "multiline title", edit: func(r *domain.DigestReview) { r.Sections[0].Items[0].Title = "first\nsecond" }},
		{name: "empty section name", edit: func(r *domain.DigestReview) { r.Sections[0].Name = " \t" }},
		{name: "empty item review", edit: func(r *domain.DigestReview) { r.Sections[0].Items[0].Review = " \n " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			review := cloneReview(valid)
			tc.edit(&review)
			if err := ValidateReview(input, review); err == nil {
				t.Fatal("ValidateReview() error = nil")
			}
		})
	}
	if err := ValidateReview(input, domain.DigestReview{}); err == nil {
		t.Fatal("ValidateReview() accepted a missing review for nonempty input")
	}
	if err := ValidateReview(domain.DigestInput{}, domain.DigestReview{}); err != nil {
		t.Fatalf("ValidateReview(empty) = %v", err)
	}
	if err := ValidateReview(domain.DigestInput{}, domain.DigestReview{Sections: []domain.DigestSection{{Name: "unexpected"}}}); err == nil {
		t.Fatal("ValidateReview() accepted sections for empty input")
	}
}

func TestRenderReviewUsesTrustedSourcesAndInputLimitations(t *testing.T) {
	input := domain.DigestInput{
		Start:  time.Date(2024, 3, 4, 9, 0, 0, 0, time.FixedZone("EST", -5*60*60)),
		Cutoff: time.Date(2024, 3, 11, 9, 0, 0, 0, time.FixedZone("EDT", -4*60*60)), Timezone: "America/New_York",
		Entries: []domain.DigestEntry{
			{JobID: 1, Analysis: domain.Analysis{Uncertainties: []string{"estimate may change"}}, Sources: []domain.DigestSource{
				{ID: "a", Title: "Readable", RequestedURL: "https://requested.example", URL: "https://trusted.example/page", Usable: true, Truncated: true},
				{ID: "failed", Title: "Blocked", RequestedURL: "https://blocked.example", Status: "restricted", Reason: "access denied"},
			}},
			{JobID: 2, Sources: []domain.DigestSource{{ID: "b", Title: "Same page", URL: "https://trusted.example/page", Status: "ok", Usable: true}}},
			{JobID: 3, Analysis: domain.Analysis{Uncertainties: []string{"historical input has no source"}}},
		},
	}
	review := domain.DigestReview{
		Opening: "Opening text", Closing: "Closing text",
		Sections: []domain.DigestSection{{Name: "Highlights", Items: []domain.DigestItem{
			{EntryIDs: []int64{1}, Title: "First item", Review: "First review", Refs: []domain.DigestRef{{JobID: 1, SourceID: "a"}}},
			{EntryIDs: []int64{2}, Title: "Second item", Review: "Second review", Refs: []domain.DigestRef{{JobID: 2, SourceID: "b"}}},
			{EntryIDs: []int64{3}, Title: "No source", Review: "Third review"},
		}}},
	}
	if err := ValidateReview(input, review); err != nil {
		t.Fatal(err)
	}
	text := Render(Report{Version: domain.DigestVersion, Input: &input, Review: &review})
	for _, want := range []string{
		"本周回顾（3条）", "覆盖区间：从 2024-03-04 09:00（含）至 2024-03-11 09:00（不含）（America/New_York）",
		"Opening text", "【Highlights】", "First item", "Second review", "Closing text",
		"https://trusted.example/page", "已知不确定性：estimate may change", "来源限制：Readable 仅读取到截断片段。",
		"来源限制：Blocked，access denied。", "来源限制：没有可用来源证据。", "已知不确定性：historical input has no source",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("new report missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "https://trusted.example/page") != 2 || strings.Contains(text, "requested.example") || strings.Contains(text, "blocked.example") {
		t.Errorf("citations were not trusted and deduplicated within each item:\n%s", text)
	}
	for _, forbidden := range []string{"提交 ", "待处理", "来源读取失败 1", "失败 1", "raw source body"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("new report exposed forbidden content %q:\n%s", forbidden, text)
		}
	}
}

func TestRenderIncompleteReviewIsNotAnEmptyReview(t *testing.T) {
	input := domain.DigestInput{Timezone: "UTC", Start: time.Now().Add(-time.Hour), Cutoff: time.Now(), Entries: []domain.DigestEntry{{JobID: 1}}}
	text := Render(Report{Version: domain.DigestVersion, Input: &input})
	if !strings.Contains(text, "周报生成未完成：尚未生成回顾内容") || strings.Contains(text, "本周回顾（0条）") || strings.Contains(text, "本周期没有") {
		t.Fatalf("nil review looked like a valid empty week: %s", text)
	}
	withoutInput := Render(Report{Version: domain.DigestVersion})
	if !strings.Contains(withoutInput, "周报生成未完成：尚无已保存的回顾材料") || strings.Contains(withoutInput, "本周回顾（0条）") {
		t.Fatalf("missing input was not reported: %s", withoutInput)
	}
	versionless := Render(Report{Input: &input, Review: &domain.DigestReview{}})
	if !strings.Contains(versionless, "周报版本不受支持") {
		t.Fatalf("versionless report was rendered: %s", versionless)
	}
	invalid := domain.DigestReview{Sections: []domain.DigestSection{{Name: "bad"}}}
	invalidText := Render(Report{Version: domain.DigestVersion, Input: &input, Review: &invalid})
	if !strings.Contains(invalidText, "未通过校验") || strings.Contains(invalidText, "本周回顾（0条）") {
		t.Fatalf("invalid review was rendered as a snapshot: %s", invalidText)
	}
}

func TestRenderEmptyReviewIsExplicit(t *testing.T) {
	input := domain.DigestInput{Timezone: "Asia/Shanghai", Start: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), Cutoff: time.Date(2024, 2, 5, 0, 0, 0, 0, time.UTC)}
	review := domain.DigestReview{}
	text := Render(Report{Version: domain.DigestVersion, Input: &input, Review: &review})
	if !strings.Contains(text, "本周回顾（0条）") || !strings.Contains(text, "所选时间范围内暂无可汇总的新内容") || !strings.Contains(text, "覆盖区间：从 2024-02-01 08:00（含）至 2024-02-05 08:00（不含）（Asia/Shanghai）") || strings.Contains(text, "周报生成未完成") {
		t.Fatalf("empty review was not rendered explicitly: %s", text)
	}
}

func TestWindowPreservesLocalWallTimeAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2024, 3, 11, 12, 34, 0, 0, loc)
	start, end := Window(cutoff, loc)
	if !end.Equal(cutoff) || !start.Equal(time.Date(2024, 3, 4, 12, 34, 0, 0, loc)) || end.Sub(start) != 167*time.Hour {
		t.Fatalf("Window() = [%s, %s), duration %s", start, end, end.Sub(start))
	}
}

func reviewInput() domain.DigestInput {
	return domain.DigestInput{Entries: []domain.DigestEntry{
		{JobID: 1, Sources: []domain.DigestSource{{ID: "a", Usable: true}}},
		{JobID: 2, Sources: []domain.DigestSource{{ID: "b", Usable: true}}},
		{JobID: 3, Sources: []domain.DigestSource{{ID: "failed", Usable: false}}},
	}}
}

func cloneReview(review domain.DigestReview) domain.DigestReview {
	out := review
	out.Sections = make([]domain.DigestSection, len(review.Sections))
	for i, section := range review.Sections {
		out.Sections[i] = section
		out.Sections[i].Items = make([]domain.DigestItem, len(section.Items))
		for j, item := range section.Items {
			out.Sections[i].Items[j] = item
			out.Sections[i].Items[j].EntryIDs = append([]int64(nil), item.EntryIDs...)
			out.Sections[i].Items[j].Refs = append([]domain.DigestRef(nil), item.Refs...)
		}
	}
	return out
}
