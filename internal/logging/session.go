package logging

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Options struct {
	Directory   string
	StoreID     string
	InstanceID  string
	Console     io.Writer
	Level       zapcore.Level
	RotateBytes int64
	MaxBytes    int64
	MaxAge      time.Duration
	Secrets     []string
}

type Session struct {
	Logger *zap.Logger

	dir             string
	storeID         string
	instanceID      string
	console         io.Writer
	level           zapcore.Level
	rotateBytes     int64
	maxBytes        int64
	maxAge          time.Duration
	secrets         []string
	file            *os.File
	filePath        string
	fileCreatedAt   time.Time
	totalBytes      int64
	seq             uint64
	lostRecords     uint64
	lastWriteReport time.Time
	lastConsoleErr  time.Time
	writeFile       func(*os.File, []byte) (int, error)

	mu        sync.Mutex
	closed    bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func Open(options Options) (*Session, error) {
	return open(options, func(file *os.File, record []byte) (int, error) { return file.Write(record) })
}

func open(options Options, writeFile func(*os.File, []byte) (int, error)) (*Session, error) {
	if strings.TrimSpace(options.Directory) == "" {
		return nil, errors.New("logging directory is required")
	}
	if options.RotateBytes <= 0 {
		options.RotateBytes = defaultRotateBytes
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultMaxBytes
	}
	if options.MaxAge <= 0 {
		options.MaxAge = defaultMaxAge
	}
	if options.RotateBytes > options.MaxBytes {
		options.RotateBytes = options.MaxBytes
	}
	if options.InstanceID == "" {
		options.InstanceID = NewID()
	}
	if options.Console == nil {
		options.Console = os.Stderr
	}

	dir := filepath.Clean(options.Directory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create logging directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect logging directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("logging path must be a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("set logging directory permissions: %w", err)
	}

	secrets := make([]string, 0, len(options.Secrets))
	for _, secret := range options.Secrets {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	s := &Session{
		dir: dir, storeID: options.StoreID, instanceID: options.InstanceID,
		console: options.Console, level: options.Level,
		rotateBytes: options.RotateBytes, maxBytes: options.MaxBytes,
		maxAge: options.MaxAge, secrets: secrets,
		stop: make(chan struct{}), done: make(chan struct{}), closeDone: make(chan struct{}),
		writeFile: writeFile,
	}
	if err := s.pruneLocked(0); err != nil {
		return nil, fmt.Errorf("prepare logging directory: %w", err)
	}
	if err := s.openActiveLocked(); err != nil {
		return nil, fmt.Errorf("open logging file: %w", err)
	}
	if err := s.verifyActiveWriteLocked(); err != nil {
		path := s.filePath
		closeErr := s.closeActiveLocked()
		removeErr := os.Remove(path)
		return nil, fmt.Errorf("verify logging file write: %w", errors.Join(err, closeErr, removeErr))
	}
	s.Logger = zap.New(&sessionCore{session: s})
	go s.housekeep()
	return s, nil
}

func DefaultDirectory(databasePath string) string {
	extension := filepath.Ext(databasePath)
	return strings.TrimSuffix(databasePath, extension) + ".logs"
}

func NewID() string { return rand.Text() }

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		s.mu.Unlock()
		<-s.done
		s.mu.Lock()
		if s.file != nil {
			s.closeErr = errors.Join(s.file.Sync(), s.file.Close())
			s.file, s.filePath = nil, ""
		}
		s.mu.Unlock()
		close(s.closeDone)
	})
	<-s.closeDone
	return s.closeErr
}

func (s *Session) housekeep() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.closed {
				if err := s.cleanupLocked(); err != nil {
					s.reportLocked("[logging] log maintenance failed; reason=" + classifyLogError(err) + "\n")
				}
			}
			s.mu.Unlock()
		}
	}
}

type sessionCore struct {
	session *Session
	fields  []zap.Field
}

func (c *sessionCore) Enabled(level zapcore.Level) bool {
	return level >= zapcore.InfoLevel || level >= c.session.level
}

func (c *sessionCore) With(fields []zap.Field) zapcore.Core {
	cloned := &sessionCore{session: c.session, fields: append([]zap.Field(nil), c.fields...)}
	cloned.fields = append(cloned.fields, fields...)
	return cloned
}

func (c *sessionCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *sessionCore) Write(entry zapcore.Entry, fields []zap.Field) error {
	c.session.writeEntry(entry, append(append([]zap.Field(nil), c.fields...), fields...))
	return nil
}

func (c *sessionCore) Sync() error {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	if c.session.file == nil {
		return nil
	}
	return c.session.file.Sync()
}

func (s *Session) writeEntry(entry zapcore.Entry, fields []zap.Field) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.seq++
	base := map[string]any{}
	encoded, err := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		MessageKey: "msg", LevelKey: "level", TimeKey: "timestamp",
		EncodeLevel: zapcore.LowercaseLevelEncoder, EncodeTime: zapcore.RFC3339NanoTimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	}).EncodeEntry(entry, fields)
	if err != nil {
		s.lostRecords++
		s.reportLocked("[logging] record could not be encoded; reason=encode_error\n")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded.Bytes()))
	decoder.UseNumber()
	decodeErr := decoder.Decode(&base)
	encoded.Free()
	if decodeErr != nil {
		s.lostRecords++
		s.reportLocked("[logging] record could not be encoded; reason=encode_error\n")
		return
	}
	cleaned := redactValue(base, s.secrets).(map[string]any)
	message, _ := cleaned["msg"].(string)
	timestamp := entry.Time
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	cleaned["schema_version"] = 1
	cleaned["store_id"] = redactValue(s.storeID, s.secrets)
	cleaned["instance_id"] = redactValue(s.instanceID, s.secrets)
	cleaned["timestamp"] = timestamp.UTC().Format(time.RFC3339Nano)
	cleaned["msg"] = message
	cleaned["level"] = entry.Level.String()
	cleaned["seq"] = s.seq
	lostBefore := s.lostRecords
	if lostBefore > 0 {
		cleaned["logging_recovered"] = true
		cleaned["logging_lost_records"] = lostBefore
	}

	fileErr := error(nil)
	if entry.Level >= zapcore.InfoLevel {
		var record []byte
		record, fileErr = json.Marshal(cleaned)
		if fileErr == nil {
			record = append(record, '\n')
			fileErr = s.writeRecordLocked(record)
		}
		if fileErr != nil {
			s.lostRecords++
			s.reportWriteFailureLocked(fileErr)
		} else if lostBefore > 0 {
			s.lostRecords = 0
			s.writeConsoleDiagnosticLocked(fmt.Sprintf("[logging] logging recovered after %d lost records\n", lostBefore))
		}
	}
	if entry.Level >= s.level {
		// A prospective recovery marker belongs on disk only if this write succeeds.
		if fileErr != nil || entry.Level < zapcore.InfoLevel {
			delete(cleaned, "logging_recovered")
			delete(cleaned, "logging_lost_records")
		}
		s.writeConsoleLocked(cleaned)
	}
}

func (s *Session) writeRecordLocked(record []byte) error {
	if err := s.ensureRecordCapacityLocked(int64(len(record))); err != nil {
		return err
	}
	startInfo, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect active log file: %w", err)
	}
	start := startInfo.Size()
	n, writeErr := s.writeFile(s.file, record)
	if writeErr == nil && n != len(record) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		if rollbackErr := s.file.Truncate(start); rollbackErr != nil {
			// Stop appending to a damaged tail; it remains an owned file for retention cleanup.
			_ = s.closeActiveLocked()
		} else {
			_, _ = s.file.Seek(start, io.SeekStart)
		}
		return fmt.Errorf("write log record: %w", writeErr)
	}
	s.totalBytes += int64(n)
	return nil
}

var consoleMetadataKeys = map[string]struct{}{
	"component": {}, "operation": {}, "event": {}, "worker": {}, "model": {},
	"storeid": {}, "instanceid": {}, "eventid": {}, "jobid": {}, "runid": {}, "taskid": {},
	"stage": {}, "attempt": {}, "durationms": {}, "count": {}, "total": {}, "sourcecount": {},
	"errortype": {}, "errorcode": {}, "httpstatus": {}, "reason": {}, "seq": {},
	"loggingrecovered": {}, "logginglostrecords": {},
	"outcome": {}, "status": {}, "taskstatus": {}, "runstatus": {}, "eventkind": {}, "runkind": {},
	"phase": {}, "elapsedms": {}, "deliveryid": {}, "receipt": {}, "apicode": {},
	"consecutivefailures": {}, "retryafter": {}, "revision": {}, "modelcallid": {}, "toolcallid": {},
	"parts": {}, "maxattempts": {}, "taskconcurrency": {}, "remainingms": {},
	"proposed": {}, "applied": {}, "skipped": {}, "proposedcount": {}, "appliedcount": {}, "skippedcount": {},
	"usagereported": {}, "inputtokens": {}, "outputtokens": {},
	"toolname": {}, "totaltokens": {}, "eventstatus": {}, "conflictcount": {}, "replypartcount": {},
	"requeuedevents": {}, "requeuedruns": {}, "uncertainsends": {}, "silent": {}, "uncertain": {},
	"updateid": {}, "messageid": {},
	"logdirectory": {},
}

func (s *Session) writeConsoleLocked(record map[string]any) {
	line := consoleSummary(record)
	if err := writeFull(s.console, line); err != nil && time.Since(s.lastConsoleErr) >= 10*time.Second {
		s.lastConsoleErr = time.Now()
		_, _ = io.WriteString(os.Stderr, "[logging] console output failed\n")
	}
}

func consoleSummary(record map[string]any) []byte {
	var b strings.Builder
	timestamp, _ := record["timestamp"].(string)
	level, _ := record["level"].(string)
	message, _ := record["msg"].(string)
	if isStructuredJSON(message) {
		message = "[structured message omitted]"
	}
	if len(message) > 240 {
		message = message[:240] + "…"
	}
	b.WriteString(timestamp)
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(level))
	b.WriteByte(' ')
	b.WriteString(strconv.Quote(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == 0 {
			return ' '
		}
		return r
	}, message)))
	keys := make([]string, 0, len(record))
	for key := range record {
		if _, ok := consoleMetadataKeys[normalizeKey(key)]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, ok := consoleScalar(record[key])
		if !ok {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(value)
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

func consoleScalar(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		if len(value) > 128 {
			value = value[:128] + "…"
		}
		return strconv.Quote(strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == 0 {
				return ' '
			}
			return r
		}, value)), true
	case json.Number:
		return value.String(), true
	case bool:
		return strconv.FormatBool(value), true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(value), true
	default:
		return "", false
	}
}

func isStructuredJSON(value string) bool {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) != nil {
		return false
	}
	switch decoded.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

func (s *Session) reportWriteFailureLocked(err error) {
	s.reportLocked("[logging] file write failed; reason=" + classifyLogError(err) + "\n")
}

func classifyLogError(err error) string {
	switch {
	case errors.Is(err, errRecordTooLarge):
		return "record_too_large"
	case errors.Is(err, errMaxStorage):
		return "storage_cap_reached"
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "permission_denied"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "no_space"
	default:
		return "io_error"
	}
}

func (s *Session) reportLocked(message string) {
	if time.Since(s.lastWriteReport) < 10*time.Second {
		return
	}
	s.lastWriteReport = time.Now()
	s.writeConsoleDiagnosticLocked(message)
}

func (s *Session) writeConsoleDiagnosticLocked(message string) {
	if err := writeFull(s.console, []byte(message)); err != nil {
		_, _ = io.WriteString(os.Stderr, "[logging] diagnostic output failed\n")
	}
}

func writeFull(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		n, err := writer.Write(value)
		if n > 0 {
			value = value[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
