package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	LevelTrace = slog.Level(-8)
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

type Fields map[string]any

var (
	defaultLogger     *Logger
	seqWriterInstance *SeqWriter
	once              sync.Once
)

type Config struct {
	LogFilePath      string
	SeqServerURL     string
	SeqAPIKey        string
	ConsoleLevel     slog.Level
	FileLevel        slog.Level
	SeqBatchSize     int
	SeqBatchInterval time.Duration
}

func Init(cfg Config) error {
	var err error
	once.Do(func() {
		optsConsole := &slog.HandlerOptions{
			Level: cfg.ConsoleLevel,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.LevelKey && a.Value.Any().(slog.Level) == LevelTrace {
					return slog.Attr{Key: slog.LevelKey, Value: slog.StringValue("TRACE")}
				}
				return a
			},
		}
		consoleHandler := slog.NewTextHandler(os.Stdout, optsConsole)

		var fileHandler slog.Handler
		if cfg.LogFilePath != "" {
			f, e := os.OpenFile(cfg.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if e != nil {
				err = e
				return
			}
			fileHandler = slog.NewJSONHandler(f, &slog.HandlerOptions{Level: cfg.FileLevel})
		} else {
			fileHandler = slog.NewJSONHandler(io.Discard, nil)
		}

		var seqHandler slog.Handler
		if cfg.SeqServerURL != "" {
			if cfg.SeqBatchSize == 0 {
				cfg.SeqBatchSize = 50
			}
			if cfg.SeqBatchInterval == 0 {
				cfg.SeqBatchInterval = 2 * time.Second
			}
			seqWriter := newSeqWriter(cfg.SeqServerURL, cfg.SeqAPIKey, cfg.SeqBatchSize, cfg.SeqBatchInterval)
			seqHandler = slog.NewJSONHandler(seqWriter, &slog.HandlerOptions{
				Level: LevelTrace,
				ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
					return a
				},
			})
		} else {
			seqHandler = slog.NewJSONHandler(io.Discard, nil)
		}

		multi := &FanoutHandler{
			handlers: []slog.Handler{consoleHandler, fileHandler, seqHandler},
		}
		logger := slog.New(multi)
		defaultLogger = &Logger{logger: logger}
		slog.SetDefault(logger)
	})
	return err
}

func Close() {
	if seqWriterInstance != nil {
		seqWriterInstance.Close()
	}
}

func ParseLevel(text string) (slog.Level, error) {
	switch strings.ToLower(text) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	case "fatal", "panic":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level '%s'", text)
	}
}

var LevelNames = map[slog.Leveler]string{
	LevelTrace: "TRACE",
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

type Logger struct {
	logger *slog.Logger
}

func (l *Logger) WithFields(f Fields) *Logger {
	attrs := make([]any, 0, len(f)*2)
	for k, v := range f {
		attrs = append(attrs, k, v)
	}
	return &Logger{logger: l.logger.With(attrs...)}
}

func (l *Logger) WithField(key string, value any) *Logger {
	return &Logger{logger: l.logger.With(key, value)}
}

func (l *Logger) WithError(err error) *Logger {
	return &Logger{logger: l.logger.With("error", err)}
}

func (l *Logger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.Add(args...)
	_ = l.logger.Handler().Handle(ctx, r)
}

func (l *Logger) Tracef(format string, args ...any) {
	l.log(context.Background(), LevelTrace, fmt.Sprintf(format, args...))
}

func (l *Logger) Trace(msg string, args ...any) {
	l.log(context.Background(), LevelTrace, msg, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	l.log(context.Background(), LevelDebug, fmt.Sprintf(format, args...))
}

func (l *Logger) Debug(msg string, args ...any) {
	l.log(context.Background(), LevelDebug, msg, args...)
}

func (l *Logger) Infof(format string, args ...any) {
	l.log(context.Background(), LevelInfo, fmt.Sprintf(format, args...))
}

func (l *Logger) Info(msg string, args ...any) {
	l.log(context.Background(), LevelInfo, msg, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.log(context.Background(), LevelWarn, fmt.Sprintf(format, args...))
}

func (l *Logger) Warn(msg string, args ...any) {
	l.log(context.Background(), LevelWarn, msg, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.log(context.Background(), LevelError, fmt.Sprintf(format, args...))
}

func (l *Logger) Error(msg string, args ...any) {
	l.log(context.Background(), LevelError, msg, args...)
}

func (l *Logger) Fatalf(format string, args ...any) {
	l.log(context.Background(), LevelError, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func (l *Logger) Fatal(msg string, args ...any) {
	l.log(context.Background(), LevelError, msg, args...)
	os.Exit(1)
}

func ensureInit() {
	if defaultLogger == nil {
		defaultLogger = &Logger{logger: slog.New(slog.NewTextHandler(os.Stdout, nil))}
	}
}

func WithFields(f Fields) *Logger {
	ensureInit()
	return defaultLogger.WithFields(f)
}
func WithField(k string, v any) *Logger {
	ensureInit()
	return defaultLogger.WithField(k, v)
}
func WithError(err error) *Logger {
	ensureInit()
	return defaultLogger.WithError(err)
}

func Tracef(format string, args ...any) {
	ensureInit()
	defaultLogger.Tracef(format, args...)
}
func Trace(msg string, args ...any) {
	ensureInit()
	defaultLogger.Trace(msg, args...)
}
func Debugf(format string, args ...any) {
	ensureInit()
	defaultLogger.Debugf(format, args...)
}
func Debug(msg string, args ...any) {
	ensureInit()
	defaultLogger.Debug(msg, args...)
}
func Infof(format string, args ...any) {
	ensureInit()
	defaultLogger.Infof(format, args...)
}
func Info(msg string, args ...any) {
	ensureInit()
	defaultLogger.Info(msg, args...)
}
func Warnf(format string, args ...any) {
	ensureInit()
	defaultLogger.Warnf(format, args...)
}
func Warn(msg string, args ...any) {
	ensureInit()
	defaultLogger.Warn(msg, args...)
}
func Errorf(format string, args ...any) {
	ensureInit()
	defaultLogger.Errorf(format, args...)
}
func Error(msg string, args ...any) {
	ensureInit()
	defaultLogger.Error(msg, args...)
}
func Fatalf(format string, args ...any) {
	ensureInit()
	defaultLogger.Errorf(format, args...)
}
func Fatal(msg string, args ...any) {
	ensureInit()
	defaultLogger.Error(msg, args...)
}

type FanoutHandler struct {
	handlers []slog.Handler
}

func (h *FanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (h *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errors []error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			err := handler.Handle(ctx, r)
			if err != nil {
				errors = append(errors, err)
			}
		}
	}
	if len(errors) > 0 {
		var err error
		for _, e := range errors {
			err = fmt.Errorf("%s\n%s", err, e)
		}
		return err
	}
	return nil
}

func (h *FanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithAttrs(attrs)
	}
	return &FanoutHandler{handlers: newHandlers}
}

func (h *FanoutHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithGroup(name)
	}
	return &FanoutHandler{handlers: newHandlers}
}

type SeqWriter struct {
	url    string
	apiKey string
	client *http.Client

	logChan chan []byte
	done    chan struct{}
	wg      sync.WaitGroup
}

func newSeqWriter(serverURL, apiKey string, batchSize int, interval time.Duration) *SeqWriter {
	s := &SeqWriter{
		url:     fmt.Sprintf("%s/api/evvents/raw", serverURL),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 10 * time.Second},
		logChan: make(chan []byte, 4096),
		done:    make(chan struct{}),
	}

	s.wg.Add(1)
	go s.run(batchSize, interval)
	return s
}

func (s *SeqWriter) Write(p []byte) (n int, err error) {
	data := make([]byte, len(p))
	copy(data, p)

	select {
	case s.logChan <- data:
		return len(p), nil
	case <-s.done:
		return 0, io.ErrClosedPipe
	default:
		return 0, fmt.Errorf("seq buffer full, dropping log")
	}
}

func (s *SeqWriter) Close() error {
	close(s.done)
	s.wg.Wait()
	return nil
}

func (s *SeqWriter) run(batchSize int, interval time.Duration) {
	defer s.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var batch [][]byte

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.sendBatch(batch)
		batch = nil
	}

	for {
		select {
		case log := <-s.logChan:
			batch = append(batch, log)
			if len(batch) == batchSize {
				flush()
				ticker.Reset(interval)
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			for len(s.logChan) > 0 {
				batch = append(batch, <-s.logChan)
			}
			flush()
			return
		}
	}
}

func (s *SeqWriter) sendBatch(logs [][]byte) {
	payload := bytes.Join(logs, []byte(""))

	req, err := http.NewRequest("POST", s.url, bytes.NewBuffer(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		req.Header.Set("X-Seq-ApiKey", s.apiKey)
	}

	resp, err := s.client.Do(req)
	if err == nil {
		defer resp.Body.Close()
	}
}
