package digest

import (
	"fmt"
	"strings"
	"time"

	"kagari/internal/domain"
)

func Render(report Report) string {
	if report.Version != domain.DigestVersion {
		return "周报生成未完成：周报版本不受支持。\n"
	}
	return renderReview(report)
}

func oneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func renderReview(report Report) string {
	if report.Input == nil {
		return "周报生成未完成：尚无已保存的回顾材料。\n"
	}
	if report.Review == nil {
		return "周报生成未完成：尚未生成回顾内容。\n"
	}
	if ValidateReview(*report.Input, *report.Review) != nil {
		return "周报生成未完成：回顾结果未通过校验。\n"
	}
	input, review := report.Input, report.Review
	var out strings.Builder
	fmt.Fprintf(&out, "本周回顾（%d条）\n", Count(*review))
	loc, err := time.LoadLocation(input.Timezone)
	if err != nil {
		loc = time.UTC
	}
	fmt.Fprintf(&out, "覆盖区间：从 %s（含）至 %s（不含）（%s）\n", input.Start.In(loc).Format("2006-01-02 15:04"), input.Cutoff.In(loc).Format("2006-01-02 15:04"), input.Timezone)
	if len(input.Entries) == 0 {
		out.WriteString("所选时间范围内暂无可汇总的新内容。\n")
		return out.String()
	}
	if strings.TrimSpace(review.Opening) != "" {
		fmt.Fprintf(&out, "\n%s\n", strings.TrimSpace(review.Opening))
	}
	entries := make(map[int64]domain.DigestEntry, len(input.Entries))
	for _, entry := range input.Entries {
		entries[entry.JobID] = entry
	}
	for _, section := range review.Sections {
		fmt.Fprintf(&out, "\n【%s】\n", oneLine(section.Name))
		for _, item := range section.Items {
			fmt.Fprintf(&out, "\n%s\n%s\n", oneLine(item.Title), strings.TrimSpace(item.Review))
			displayedURLs := make(map[string]bool)
			for _, ref := range item.Refs {
				entry, ok := entries[ref.JobID]
				if !ok {
					continue
				}
				for _, source := range entry.Sources {
					if source.ID != ref.SourceID || !source.Usable {
						continue
					}
					url := source.URL
					if strings.TrimSpace(url) == "" {
						url = source.RequestedURL
					}
					url = strings.TrimSpace(url)
					if url != "" {
						if displayedURLs[url] {
							continue
						}
						displayedURLs[url] = true
						label := oneLine(source.Title)
						if label == "" || label == url {
							fmt.Fprintf(&out, "引用：%s\n", url)
						} else {
							fmt.Fprintf(&out, "引用：%s — %s\n", label, url)
						}
					} else if label := oneLine(source.Title); label != "" {
						fmt.Fprintf(&out, "引用：%s（无可用链接）\n", label)
					}
				}
			}
			renderLimitations(&out, item.EntryIDs, entries)
		}
	}
	if strings.TrimSpace(review.Closing) != "" {
		fmt.Fprintf(&out, "\n%s\n", strings.TrimSpace(review.Closing))
	}
	return out.String()
}

func renderLimitations(out *strings.Builder, ids []int64, entries map[int64]domain.DigestEntry) {
	for _, id := range ids {
		entry, ok := entries[id]
		if !ok {
			continue
		}
		for _, uncertainty := range entry.Analysis.Uncertainties {
			if value := oneLine(uncertainty); value != "" {
				fmt.Fprintf(out, "已知不确定性：%s\n", value)
			}
		}
		usable := false
		for _, source := range entry.Sources {
			label := oneLine(source.Title)
			if label == "" {
				label = oneLine(source.URL)
			}
			if label == "" {
				label = "来源"
			}
			if source.Usable {
				usable = true
				if source.Truncated {
					fmt.Fprintf(out, "来源限制：%s 仅读取到截断片段。\n", label)
				}
				continue
			}
			detail := oneLine(source.Reason)
			if detail == "" {
				detail = oneLine(source.Status)
			}
			if detail == "" {
				detail = "未取得可用正文"
			}
			fmt.Fprintf(out, "来源限制：%s，%s。\n", label, detail)
		}
		if !usable {
			out.WriteString("来源限制：没有可用来源证据。\n")
		}
	}
}
