package documentarchive

import (
	"context"
	"errors"
	"io"
	"time"

	"kagari/internal/textpage"
)

var (
	ErrNotFound       = errors.New("archive document or version not found")
	ErrConflict       = errors.New("archive version conflicts with an existing version")
	ErrSummaryPending = errors.New("archive summary is still being prepared")
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
	StatusNeedsNote  Status = "needs_note"
)

type SummarySource string

const (
	SummaryFromOriginal SummarySource = "original"
	SummaryFromOwner    SummarySource = "owner_note"
)

type Request struct {
	EventID       int64
	RequestKey    string
	AttachmentID  string
	SourceEventID int64
	DocumentID    int64
	Title         string
	Category      string
	Tags          []string
	Note          string
	Number        int
	Historical    bool
}

type Document struct {
	ID       int64    `json:"document_id"`
	Title    string   `json:"title"`
	Category string   `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

type Version struct {
	ID               int64         `json:"version_id"`
	DocumentID       int64         `json:"document_id"`
	Number           int           `json:"number"`
	Historical       bool          `json:"historical,omitempty"`
	AttachmentID     string        `json:"-"`
	Name             string        `json:"name"`
	MIME             string        `json:"mime,omitempty"`
	Size             int64         `json:"size"`
	ObjectKey        string        `json:"-"`
	EventID          int64         `json:"archive_event_id"`
	SourceEventID    int64         `json:"source_event_id,omitempty"`
	SubmittedAt      time.Time     `json:"submitted_at"`
	Note             string        `json:"note,omitempty"`
	UploadStatus     Status        `json:"upload_status"`
	SummaryStatus    Status        `json:"summary_status"`
	Summary          string        `json:"summary,omitempty"`
	SummarySource    SummarySource `json:"summary_source,omitempty"`
	SummaryScope     string        `json:"summary_scope,omitempty"`
	ExtractedText    string        `json:"-"`
	ExtractedPartial bool          `json:"-"`
	ExtractedScope   string        `json:"-"`
}

func (v Version) Archived() bool {
	return v.UploadStatus == StatusReady && v.SummaryStatus == StatusReady
}

type Detail struct {
	Document         Document  `json:"document"`
	Latest           Version   `json:"latest"`
	Versions         []Version `json:"versions"`
	NextBeforeNumber int       `json:"next_before_number,omitempty"`
}

type Query struct {
	Text     string
	Category string
	Tag      string
	From     time.Time
	To       time.Time
	Limit    int
	Offset   int
}

type DocumentPatch struct {
	EventID    int64
	DocumentID int64
	Title      *string
	Category   *string
	Tags       *[]string
}

type Hit struct {
	Document     Document `json:"document"`
	Match        Version  `json:"match"`
	Latest       Version  `json:"latest"`
	MatchedField string   `json:"matched_field,omitempty"`
}

type WorkKind string

const (
	WorkUpload  WorkKind = "upload"
	WorkSummary WorkKind = "summary"
)

type Work struct {
	Kind    WorkKind
	Version Version
	Attempt int
}

type WorkResult struct {
	Status           Status
	RetryAt          time.Time
	Error            string
	Summary          string
	SummarySource    SummarySource
	SummaryScope     string
	ExtractedText    string
	ExtractedPartial bool
	ExtractedScope   string
}

type Store interface {
	EnqueueArchive(context.Context, Request) (Version, error)
	ArchiveVersion(context.Context, int64) (Version, error)
	ArchiveDocument(context.Context, int64, int, int) (Detail, error)
	SearchArchives(context.Context, Query) ([]Hit, error)
	UpdateArchiveDocument(context.Context, DocumentPatch) (Document, error)
	SetArchiveSummary(context.Context, int64, int64, WorkResult) (Version, error)
	ClaimArchiveWork(context.Context, time.Time) (*Work, error)
	FinishArchiveWork(context.Context, Work, WorkResult) error
	RecoverArchiveWork(context.Context) error
}

type ObjectStore interface {
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, mediaType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	DownloadURL(ctx context.Context, key, name string, ttl time.Duration) (string, error)
}

type Download struct {
	VersionID    int64     `json:"version_id"`
	UploadStatus Status    `json:"upload_status"`
	URL          string    `json:"url,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

type Original struct {
	VersionID    int64  `json:"version_id"`
	UploadStatus Status `json:"upload_status"`
	AttachmentID string `json:"attachment_id,omitempty"`
}

type Text struct {
	VersionID    int64          `json:"version_id"`
	SummaryState Status         `json:"summary_status"`
	Scope        string         `json:"scope,omitempty"`
	Page         *textpage.Page `json:"page,omitempty"`
}
