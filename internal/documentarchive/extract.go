package documentarchive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	maxSampleRunes = 12_000
	maxPDFPages    = 3
	maxDOCXXML     = 2 << 20
)

type Sample struct {
	Text    string
	Scope   string
	Partial bool
	Image   []byte
	MIME    string
}

type Summarize func(context.Context, Sample) (string, error)

func extractSample(ctx context.Context, version Version, data []byte) (Sample, error) {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/webp":
		return Sample{Image: data, MIME: mime, Scope: "整张图片"}, nil
	case "application/pdf":
		return samplePDF(ctx, data)
	}
	if strings.EqualFold(filepath.Ext(version.Name), ".docx") {
		return sampleDOCX(ctx, data)
	}
	if (strings.HasPrefix(mime, "text/plain") || strings.HasPrefix(version.MIME, "text/")) && utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return textSample(string(data), "UTF-8 正文", false), nil
	}
	return Sample{}, nil
}

func textSample(body, scope string, partial bool) Sample {
	body = strings.TrimSpace(body)
	text := clipRunes(body, maxSampleRunes)
	if len(text) < len(body) {
		scope += "（最多 12000 字符）"
		partial = true
	}
	return Sample{Text: text, Scope: scope, Partial: partial}
}

func samplePDF(ctx context.Context, data []byte) (Sample, error) {
	path, err := exec.LookPath("pdftotext")
	if errors.Is(err, exec.ErrNotFound) {
		return Sample{}, nil
	}
	if err != nil {
		return Sample{}, err
	}
	output := &pdfSampleOutput{}
	cmd := exec.CommandContext(ctx, path, "-f", "1", "-l", fmt.Sprint(maxPDFPages), "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return Sample{}, fmt.Errorf("extract PDF text: %w", err)
	}
	text := strings.TrimSuffix(strings.ToValidUTF8(output.String(), ""), "\f")
	pageTexts := strings.Split(text, "\f")
	var body strings.Builder
	for i, page := range pageTexts {
		if strings.TrimSpace(page) != "" {
			fmt.Fprintf(&body, "第 %d 页\n%s\n", i+1, strings.TrimSpace(page))
		}
	}
	scope := fmt.Sprintf("PDF 第 1–%d 页的文字层", len(pageTexts))
	return textSample(body.String(), scope, output.partial || len(pageTexts) >= maxPDFPages), nil
}

type pdfSampleOutput struct {
	strings.Builder
	partial bool
}

func (b *pdfSampleOutput) Write(p []byte) (int, error) {
	available := maxSampleRunes*utf8.UTFMax - b.Len()
	_, _ = b.Builder.Write(p[:min(len(p), available)])
	b.partial = b.partial || len(p) > available
	return len(p), nil
}

func sampleDOCX(ctx context.Context, data []byte) (Sample, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Sample{}, fmt.Errorf("open Word file: %w", err)
	}
	file, err := archive.Open("word/document.xml")
	if err != nil {
		return Sample{}, fmt.Errorf("open Word document body: %w", err)
	}
	defer file.Close()
	source := &io.LimitedReader{R: file, N: maxDOCXXML}
	decoder := xml.NewDecoder(source)
	var body strings.Builder
	var inText bool
	for body.Len() <= maxSampleRunes*utf8.UTFMax {
		if err := ctx.Err(); err != nil {
			return Sample{}, err
		}
		token, err := decoder.Token()
		if err == io.EOF || (err != nil && source.N == 0) {
			return textSample(body.String(), "Word 主文档正文", source.N == 0), nil
		}
		if err != nil {
			return Sample{}, fmt.Errorf("extract Word document body: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			inText = token.Name.Local == "t" && isWordXML(token.Name.Space)
		case xml.EndElement:
			if token.Name.Local == "p" && isWordXML(token.Name.Space) {
				body.WriteByte('\n')
			}
			inText = false
		case xml.CharData:
			if inText {
				body.Write(token)
			}
		}
	}
	return textSample(body.String(), "Word 主文档正文", true), nil
}

func isWordXML(namespace string) bool {
	return namespace == "http://schemas.openxmlformats.org/wordprocessingml/2006/main" || namespace == "http://purl.oclc.org/ooxml/wordprocessingml/main"
}
