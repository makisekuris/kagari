package personal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kagari/internal/channel"
)

func (s *Store) attachmentDir(ctx context.Context) (string, error) {
	if s.baseDir == "" {
		return "", errors.New("personal: attachment storage requires a file-backed database")
	}
	id, err := s.Identity(ctx)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("personal: attachment store identity is missing")
	}
	return filepath.Join(s.baseDir, "attachments-"+id), nil
}

func (s *Store) SaveAttachment(ctx context.Context, name, mediaType string, source io.Reader) (channel.Attachment, error) {
	var result channel.Attachment
	if source == nil {
		return result, errors.New("personal: attachment contents are required")
	}
	name = safeAttachmentName(name)
	if name == "" {
		return result, errors.New("personal: attachment name is required")
	}
	mediaType = strings.TrimSpace(mediaType)
	if mediaType != "" {
		parsed, _, parseErr := mime.ParseMediaType(mediaType)
		if parseErr != nil {
			return result, fmt.Errorf("personal: invalid attachment media type: %w", parseErr)
		}
		mediaType = parsed
	}
	if _, err := s.EnsureIdentity(ctx); err != nil {
		return result, err
	}
	dir, err := s.attachmentDir(ctx)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return result, fmt.Errorf("personal: create attachment directory: %w", err)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return result, err
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("personal: attachment directory must be a real directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return result, fmt.Errorf("personal: secure attachment directory: %w", err)
	}
	var rawID [16]byte
	if _, err := rand.Read(rawID[:]); err != nil {
		return result, fmt.Errorf("personal: create attachment ID: %w", err)
	}
	id := hex.EncodeToString(rawID[:])
	finalPath := filepath.Join(dir, id+".bin")
	tmp, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return result, fmt.Errorf("personal: create attachment file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	var prefix strings.Builder
	limited := io.LimitReader(source, channel.MaxFileBytes+1)
	reader := io.TeeReader(limited, &limitedPrefix{builder: &prefix, limit: 512})
	size, copyErr := io.Copy(tmp, reader)
	if copyErr != nil {
		_ = tmp.Close()
		return result, fmt.Errorf("personal: save attachment bytes: %w", copyErr)
	}
	if size > channel.MaxFileBytes {
		_ = tmp.Close()
		return result, fmt.Errorf("personal: attachment exceeds %d bytes", channel.MaxFileBytes)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return result, fmt.Errorf("personal: sync attachment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return result, fmt.Errorf("personal: close attachment: %w", err)
	}
	if mediaType == "" {
		mediaType = mime.TypeByExtension(filepath.Ext(name))
	}
	if mediaType == "" && size > 0 {
		mediaType = http.DetectContentType([]byte(prefix.String()))
	}
	if mediaType == "application/octet-stream" {
		mediaType = ""
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return result, fmt.Errorf("personal: publish attachment: %w", err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		_ = os.Remove(finalPath)
		return result, fmt.Errorf("personal: open attachment directory: %w", err)
	}
	if err := dirHandle.Sync(); err != nil {
		_ = dirHandle.Close()
		_ = os.Remove(finalPath)
		return result, fmt.Errorf("personal: sync attachment directory: %w", err)
	}
	if err := dirHandle.Close(); err != nil {
		_ = os.Remove(finalPath)
		return result, fmt.Errorf("personal: close attachment directory: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO personal_attachments(id,name,mime,size,path,status,created_at)
		VALUES(?,?,?,?,?,'available',?)`, id, name, mediaType, size, finalPath, timestamp(time.Now()))
	if err != nil {
		_ = os.Remove(finalPath)
		return result, fmt.Errorf("personal: save attachment metadata: %w", err)
	}
	result = channel.Attachment{ID: id, Name: name, MIME: mediaType, Size: size}
	return result, nil
}

type limitedPrefix struct {
	builder *strings.Builder
	limit   int
}

type attachmentQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) refreshAttachments(ctx context.Context, query attachmentQuery, attachments []channel.Attachment) error {
	for i := range attachments {
		if attachments[i].ID == "" {
			continue
		}
		var canonical channel.Attachment
		var path, status string
		err := query.QueryRowContext(ctx, `SELECT id,name,mime,size,path,status FROM personal_attachments WHERE id=?`, attachments[i].ID).
			Scan(&canonical.ID, &canonical.Name, &canonical.MIME, &canonical.Size, &path, &status)
		if errors.Is(err, sql.ErrNoRows) {
			attachments[i].Error = "attachment metadata is unavailable"
			continue
		}
		if err != nil {
			return err
		}
		attachments[i].Name, attachments[i].MIME, attachments[i].Size = canonical.Name, canonical.MIME, canonical.Size
		switch status {
		case "deleted":
			attachments[i].Deleted, attachments[i].Error = true, "attachment was deleted"
		case "deleting":
			attachments[i].Error = "attachment deletion is incomplete"
		case "available":
			attachments[i].Deleted = false
			attachments[i].Error = ""
			if err := s.checkAttachmentFile(ctx, query, canonical.ID, path, canonical.Size); err != nil {
				attachments[i].Error = err.Error()
			}
		default:
			attachments[i].Error = "attachment is unavailable"
		}
	}
	return nil
}

func (s *Store) checkAttachmentFile(ctx context.Context, query attachmentQuery, id, path string, size int64) error {
	var storeID string
	if err := query.QueryRowContext(ctx, `SELECT value FROM personal_meta WHERE key='store_id'`).Scan(&storeID); err != nil {
		return err
	}
	if s.baseDir == "" {
		return errors.New("attachment storage directory is unavailable")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return errors.New("attachment ID is invalid")
	}
	want := filepath.Join(s.baseDir, "attachments-"+storeID, id+".bin")
	if len(id) != 32 || filepath.Clean(path) != want {
		return errors.New("attachment path is outside the controlled directory")
	}
	dirInfo, err := os.Lstat(filepath.Dir(want))
	if err != nil {
		return fmt.Errorf("stored file is unavailable: %w", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("attachment directory is not controlled")
	}
	info, err := os.Lstat(want)
	if err != nil {
		return fmt.Errorf("stored file is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != size || size > channel.MaxFileBytes {
		return errors.New("stored file failed integrity checks")
	}
	return nil
}

func (w *limitedPrefix) Write(p []byte) (int, error) {
	original := len(p)
	if left := w.limit - w.builder.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = w.builder.Write(p)
	}
	return original, nil
}

func safeAttachmentName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name))
	if name == "." || name == ".." {
		return ""
	}
	return name
}

func (s *Store) Attachment(ctx context.Context, id string) (channel.Attachment, error) {
	var attachment channel.Attachment
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,mime,size,status FROM personal_attachments WHERE id=?`, id).
		Scan(&attachment.ID, &attachment.Name, &attachment.MIME, &attachment.Size, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attachment, fmt.Errorf("%w: %s", ErrAttachmentUnavailable, id)
		}
		return attachment, err
	}
	attachment.Deleted = status == "deleted"
	if status == "deleted" {
		attachment.Error = "attachment was deleted"
	}
	if status == "deleting" {
		attachment.Error = "deletion incomplete"
	}
	if status == "available" {
		file, err := s.OpenAttachment(ctx, id)
		if err != nil {
			attachment.Error = err.Error()
		} else {
			_ = file.Close()
		}
	}
	return attachment, nil
}

func (s *Store) Attachments(ctx context.Context) ([]channel.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,mime,size,status FROM personal_attachments ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := []channel.Attachment{}
	for rows.Next() {
		var attachment channel.Attachment
		var status string
		if err := rows.Scan(&attachment.ID, &attachment.Name, &attachment.MIME, &attachment.Size, &status); err != nil {
			return nil, err
		}
		attachment.Deleted = status == "deleted"
		if status == "deleting" {
			attachment.Error = "deletion incomplete"
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.refreshAttachments(ctx, s.db, attachments); err != nil {
		return nil, err
	}
	return attachments, nil
}

func (s *Store) attachmentPath(ctx context.Context, id string) (string, string, int64, error) {
	if len(id) != 32 {
		return "", "", 0, errors.New("personal: invalid attachment ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", "", 0, errors.New("personal: invalid attachment ID")
	}
	var path, status string
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT path,status,size FROM personal_attachments WHERE id=?`, id).Scan(&path, &status, &size)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", 0, fmt.Errorf("%w: %s", ErrAttachmentUnavailable, id)
		}
		return "", "", 0, err
	}
	dir, err := s.attachmentDir(ctx)
	if err != nil {
		return "", "", 0, err
	}
	want := filepath.Join(dir, id+".bin")
	if filepath.Clean(path) != want {
		return "", "", 0, errors.New("personal: attachment path is outside its controlled directory")
	}
	dirInfo, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return want, status, size, nil
	}
	if err != nil {
		return "", "", 0, err
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", 0, errors.New("personal: attachment directory is not controlled")
	}
	return want, status, size, nil
}

func (s *Store) OpenAttachment(ctx context.Context, id string) (io.ReadCloser, error) {
	path, status, size, err := s.attachmentPath(ctx, id)
	if err != nil {
		return nil, err
	}
	if status != "available" {
		if status == "deleted" {
			return nil, fmt.Errorf("%w: %s was deleted", ErrAttachmentUnavailable, id)
		}
		return nil, fmt.Errorf("%w: %s", ErrAttachmentUnavailable, id)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s file is missing", ErrAttachmentUnavailable, id)
		}
		return nil, fmt.Errorf("personal: open attachment: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("personal: attachment is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("personal: open attachment: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() != size || opened.Size() > channel.MaxFileBytes {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s failed integrity checks", ErrAttachmentUnavailable, id)
	}
	return file, nil
}

func (s *Store) DeleteAttachment(ctx context.Context, id string) error {
	path, status, _, err := s.attachmentPath(ctx, id)
	if err != nil {
		return err
	}
	if status == "deleted" {
		return nil
	}
	if status == "available" {
		if _, err := s.db.ExecContext(ctx, `UPDATE personal_attachments SET status='deleting' WHERE id=? AND status='available'`, id); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_, restoreErr := s.db.ExecContext(ctx, `UPDATE personal_attachments SET status='available' WHERE id=? AND status='deleting'`, id)
		if restoreErr != nil {
			return fmt.Errorf("personal: remove attachment: %v; restore metadata: %w", err, restoreErr)
		}
		return fmt.Errorf("personal: remove attachment: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE personal_attachments SET status='deleted' WHERE id=? AND status='deleting'`, id)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return errors.New("personal: attachment deletion state changed unexpectedly")
	}
	return nil
}

func (s *Store) validateStoredAttachment(ctx context.Context, tx *sql.Tx, id string) (channel.Attachment, error) {
	var attachment channel.Attachment
	var path string
	var status string
	err := tx.QueryRowContext(ctx, `SELECT id,name,mime,size,path,status FROM personal_attachments WHERE id=?`, id).
		Scan(&attachment.ID, &attachment.Name, &attachment.MIME, &attachment.Size, &path, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attachment, fmt.Errorf("%w: %s", ErrAttachmentUnavailable, id)
		}
		return attachment, fmt.Errorf("personal: attachment %q: %w", id, err)
	}
	if status != "available" {
		return attachment, fmt.Errorf("%w: %s", ErrAttachmentUnavailable, id)
	}
	var storeID string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM personal_meta WHERE key='store_id'`).Scan(&storeID); err != nil {
		return attachment, err
	}
	if s.baseDir == "" {
		return attachment, errors.New("personal: attachment storage directory is unavailable")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return attachment, fmt.Errorf("personal: attachment %q has an unsafe ID", id)
	}
	want := filepath.Join(s.baseDir, "attachments-"+storeID, id+".bin")
	if len(id) != 32 || filepath.Clean(path) != want {
		return attachment, fmt.Errorf("personal: attachment %q has an unsafe path", id)
	}
	dirInfo, err := os.Lstat(filepath.Dir(want))
	if err != nil {
		return attachment, fmt.Errorf("personal: attachment %q directory is unavailable: %w", id, err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return attachment, fmt.Errorf("personal: attachment %q directory is not controlled", id)
	}
	info, err := os.Lstat(want)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return attachment, fmt.Errorf("%w: %s file is missing", ErrAttachmentUnavailable, id)
		}
		return attachment, fmt.Errorf("personal: attachment %q file is unavailable: %w", id, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != attachment.Size || info.Size() > channel.MaxFileBytes {
		return attachment, fmt.Errorf("%w: %s failed integrity checks", ErrAttachmentUnavailable, id)
	}
	return attachment, nil
}

var _ channel.Files = (*Store)(nil)
var _ = sql.ErrNoRows
