package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"kagari/internal/documentarchive"
)

type archiveSearchRow struct {
	row      scanner
	document *documentarchive.Document
	rawTags  *string
}

func (r archiveSearchRow) Scan(dest ...any) error {
	args := append(dest, &r.document.ID, &r.document.Title, &r.document.Category, r.rawTags)
	return r.row.Scan(args...)
}

type archiveSearchCandidate struct {
	document documentarchive.Document
	match    documentarchive.Version
	fields   string
}

func archiveSearchTerms(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	var terms []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		term := current.String()
		for _, existing := range terms {
			if existing == term {
				current.Reset()
				return
			}
		}
		terms = append(terms, term)
		current.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			current.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(terms) == 0 {
		return []string{text}
	}
	return terms
}

func archiveVersionMatchedFields(document documentarchive.Document, version documentarchive.Version, terms []string) (bool, string) {
	if len(terms) == 0 {
		return true, ""
	}
	fields := []struct{ name, value string }{
		{"title", document.Title},
		{"category", document.Category},
		{"tag", strings.Join(document.Tags, " ")},
		{"filename", version.Name},
		{"note", version.Note},
		{"summary", version.Summary},
		{"identifier", fmt.Sprintf("document %d document_id %d version %d version_id %d attachment %s attachment_id %s source event %d source_event_id %d event %d event_id %d",
			document.ID, document.ID, version.ID, version.ID, version.AttachmentID, version.AttachmentID,
			version.SourceEventID, version.SourceEventID, version.EventID, version.EventID)},
	}
	lower := make([]string, len(fields))
	for i := range fields {
		lower[i] = strings.ToLower(fields[i].value)
	}
	matchedFields := make([]string, 0, len(fields))
	seenFields := make(map[string]struct{}, len(fields))
	for _, term := range terms {
		matched := false
		for i, field := range fields {
			if strings.Contains(lower[i], term) {
				matched = true
				if _, ok := seenFields[field.name]; !ok {
					seenFields[field.name] = struct{}{}
					matchedFields = append(matchedFields, field.name)
				}
			}
		}
		if !matched {
			return false, ""
		}
	}
	return true, strings.Join(matchedFields, ", ")
}

func archiveMatchesTag(tags []string, want string) bool {
	if want == "" {
		return true
	}
	for _, tag := range tags {
		if strings.EqualFold(tag, want) {
			return true
		}
	}
	return false
}

func (s *Store) SearchArchives(ctx context.Context, query documentarchive.Query) ([]documentarchive.Hit, error) {
	if query.Limit < 0 || query.Offset < 0 || query.Offset > 100000 {
		return nil, errors.New("personal: archive search requires bounded nonnegative paging")
	}
	if query.Limit == 0 {
		query.Limit = defaultArchivePageSize
	} else if query.Limit > maxArchivePageSize {
		query.Limit = maxArchivePageSize
	}
	if !query.From.IsZero() && !query.To.IsZero() && query.From.After(query.To) {
		return nil, errors.New("personal: archive search start time must not follow end time")
	}
	query.Category = strings.TrimSpace(query.Category)
	query.Tag = strings.TrimSpace(query.Tag)
	terms := archiveSearchTerms(query.Text)
	category := query.Category
	from, to := "", ""
	if !query.From.IsZero() {
		from = timestamp(query.From)
	}
	if !query.To.IsZero() {
		to = timestamp(query.To)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+archiveVersionCatalogColumns+`,d.id,d.title,d.category,d.tags
		FROM personal_archive_documents d JOIN personal_archive_versions v ON v.document_id=d.id
		WHERE (?='' OR d.category=? COLLATE NOCASE) AND (?='' OR v.submitted_at>=?) AND (?='' OR v.submitted_at<=?)
		ORDER BY v.submitted_at DESC,v.id DESC`, category, category, from, from, to, to)
	if err != nil {
		return nil, err
	}
	byDocument := make(map[int64]archiveSearchCandidate)
	for rows.Next() {
		var doc documentarchive.Document
		var rawTags string
		version, err := scanArchiveVersion(archiveSearchRow{row: rows, document: &doc, rawTags: &rawTags})
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(rawTags), &doc.Tags); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("personal: decode archive tags for document %d: %w", doc.ID, err)
		}
		if doc.Tags == nil {
			doc.Tags = []string{}
		}
		if query.Category != "" && !strings.EqualFold(doc.Category, query.Category) {
			continue
		}
		if !archiveMatchesTag(doc.Tags, query.Tag) {
			continue
		}
		if !query.From.IsZero() && version.SubmittedAt.Before(query.From) {
			continue
		}
		if !query.To.IsZero() && version.SubmittedAt.After(query.To) {
			continue
		}
		matched, fields := archiveVersionMatchedFields(doc, version, terms)
		if !matched {
			continue
		}
		candidate := archiveSearchCandidate{document: doc, match: version, fields: fields}
		old, found := byDocument[doc.ID]
		if !found || version.Number > old.match.Number || (version.Number == old.match.Number && version.ID > old.match.ID) {
			byDocument[doc.ID] = candidate
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	candidates := make([]archiveSearchCandidate, 0, len(byDocument))
	for _, candidate := range byDocument {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].match.SubmittedAt.Equal(candidates[j].match.SubmittedAt) {
			return candidates[i].document.ID > candidates[j].document.ID
		}
		return candidates[i].match.SubmittedAt.After(candidates[j].match.SubmittedAt)
	})
	start := query.Offset
	if start >= len(candidates) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return []documentarchive.Hit{}, nil
	}
	end := start + query.Limit
	if end > len(candidates) {
		end = len(candidates)
	}
	hits := make([]documentarchive.Hit, 0, end-start)
	for _, candidate := range candidates[start:end] {
		doc, latestID, err := loadArchiveDocumentTx(ctx, tx, candidate.document.ID)
		if err != nil {
			return nil, err
		}
		latest, err := scanArchiveVersion(tx.QueryRowContext(ctx, `SELECT `+archiveVersionCatalogColumns+`
			FROM personal_archive_versions v WHERE v.id=?`, latestID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, documentarchive.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		hits = append(hits, documentarchive.Hit{Document: doc, Match: candidate.match, Latest: latest, MatchedField: candidate.fields})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return hits, nil
}
