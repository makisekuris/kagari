package documentarchive

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestArchiveExtractsWordBody(t *testing.T) {
	var file bytes.Buffer
	zipFile := zip.NewWriter(&file)
	part, err := zipFile.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = part.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		`<w:p><w:r><w:t>厨房报价</w:t></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>防水五年</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
		`</w:body></w:document>`))
	if err != nil {
		t.Fatal(err)
	}
	if err := zipFile.Close(); err != nil {
		t.Fatal(err)
	}
	sample, err := extractSample(t.Context(), Version{Name: "报价.docx"}, file.Bytes())
	if err != nil || sample.Text != "厨房报价\n防水五年" || sample.Scope != "Word 主文档正文" || sample.Partial {
		t.Fatalf("unexpected Word sample %#v: %v", sample, err)
	}
}

func TestArchiveSamplesBoundedTextAndOwnerEvidence(t *testing.T) {
	sample, err := extractSample(t.Context(), Version{Name: "报价.txt", MIME: "text/plain"}, []byte(strings.Repeat("价", maxSampleRunes+1)))
	if err != nil || !sample.Partial || utf8.RuneCountInString(sample.Text) != maxSampleRunes {
		t.Fatalf("unexpected sampled length %d or metadata: %v", utf8.RuneCountInString(sample.Text), err)
	}
	if status := ownerSummary("").Status; status != StatusNeedsNote {
		t.Fatalf("empty evidence set status to %s", status)
	}
	result := ownerSummary(" " + strings.Repeat("价", maxSummaryRunes+1) + " ")
	if result.Status != StatusReady || result.SummarySource != SummaryFromOwner || utf8.RuneCountInString(result.Summary) != maxSummaryRunes {
		t.Fatal("owner evidence must remain labeled and bounded")
	}
}
