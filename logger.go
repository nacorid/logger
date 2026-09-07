package logger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	slogseq "github.com/sokkalf/slog-seq"
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
	mu            sync.RWMutex
	defaultLogger = &Logger{logger: slog.Default()}
	closers       []io.Closer
	packagePrefix string
)

func init() {

	var pcs [1]uintptr
	runtime.Callers(1, pcs[:])
	fn := runtime.FuncForPC(pcs[0]).Name()
	lastSlash := strings.LastIndex(fn, "/")
	if lastSlash == -1 {
		lastSlash = 0
	}
	dot := strings.Index(fn[lastSlash:], ".")
	if dot != -1 {
		packagePrefix = fn[:lastSlash+dot+1]
	}
}

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
	mu.Lock()
	defer mu.Unlock()

	closeInternal()

	replace := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.SourceKey {
			if s, ok := a.Value.Any().(*slog.Source); ok && s != nil {
				s.File = filepath.Base(s.File)
				return slog.Any(a.Key, s)
			}
		}
		if a.Key == slog.LevelKey {
			if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == LevelTrace {
				return slog.String(slog.LevelKey, "TRACE")
			}
		}
		return a
	}

	optsConsole := &slog.HandlerOptions{
		Level:       cfg.ConsoleLevel,
		ReplaceAttr: replace,
		AddSource:   true,
	}
	consoleHandler := slog.NewTextHandler(os.Stdout, optsConsole)

	var fileHandler slog.Handler
	if cfg.LogFilePath != "" {
		f, err := os.OpenFile(cfg.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file: %w", err)
		}
		closers = append(closers, f)
		fileHandler = slog.NewJSONHandler(f, &slog.HandlerOptions{
			Level:       cfg.FileLevel,
			ReplaceAttr: replace,
			AddSource:   true,
		})
	} else {
		fileHandler = slog.NewJSONHandler(io.Discard, nil)
	}

	var seqHandler slog.Handler
	if cfg.SeqServerURL != "" {
		if cfg.SeqBatchSize <= 0 {
			cfg.SeqBatchSize = 50
		}
		if cfg.SeqBatchInterval <= 0 {
			cfg.SeqBatchInterval = 2 * time.Second
		}
		_, slogseqHandler := slogseq.NewLogger(
			cfg.SeqServerURL+"/ingest/clef",
			slogseq.WithAPIKey(cfg.SeqAPIKey),
			slogseq.WithBatchSize(cfg.SeqBatchSize),
			slogseq.WithFlushInterval(cfg.SeqBatchInterval),
			slogseq.WithHandlerOptions(&slog.HandlerOptions{
				Level:       LevelTrace,
				ReplaceAttr: replace,
				AddSource:   true,
			}),
		)
		closers = append(closers, slogseqHandler)
		seqHandler = slogseqHandler
	} else {
		seqHandler = slog.NewJSONHandler(io.Discard, nil)
	}

	multi := &FanoutHandler{
		handlers: []slog.Handler{consoleHandler, fileHandler, seqHandler},
	}
	logger := slog.New(multi)
	defaultLogger = &Logger{logger: logger}
	slog.SetDefault(logger)

	return nil
}

func SetDefault(l *slog.Logger) {
	mu.Lock()
	defer mu.Unlock()
	defaultLogger = &Logger{logger: l}
	slog.SetDefault(l)
}

func closeInternal() error {
	var errs []error
	for _, c := range closers {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	closers = nil
	return errors.Join(errs...)
}

func Close() error {
	mu.Lock()
	defer mu.Unlock()
	return closeInternal()
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

func getCallerPC() uintptr {
	if packagePrefix == "" {
		return 0
	}
	var pcs [20]uintptr
	n := runtime.Callers(2, pcs[:])
	if n == 0 {
		return 0
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, packagePrefix) {
			if !more {
				break
			}
			continue
		}
		return frame.PC
	}
	return 0
}

func (l *Logger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if !l.logger.Handler().Enabled(ctx, level) {
		return
	}
	r := slog.NewRecord(time.Now(), level, msg, getCallerPC())
	r.Add(args...)
	if err := l.logger.Handler().Handle(ctx, r); err != nil {
		fmt.Fprintf(os.Stderr, "logger: failed to write log record: %v\n", err)
	}
}

func (l *Logger) Tracef(format string, args ...any) {
	l.log(context.Background(), LevelTrace, fmt.Sprintf(format, args...))
}

func (l *Logger) Trace(msg string, args ...any) {
	l.log(context.Background(), LevelTrace, msg, args...)
}

func (l *Logger) TraceWithContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelTrace, msg, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	l.log(context.Background(), LevelDebug, fmt.Sprintf(format, args...))
}

func (l *Logger) Debug(msg string, args ...any) {
	l.log(context.Background(), LevelDebug, msg, args...)
}

func (l *Logger) DebugWithContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelDebug, msg, args...)
}

func (l *Logger) Infof(format string, args ...any) {
	l.log(context.Background(), LevelInfo, fmt.Sprintf(format, args...))
}

func (l *Logger) Info(msg string, args ...any) {
	l.log(context.Background(), LevelInfo, msg, args...)
}

func (l *Logger) InfoWithContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelInfo, msg, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.log(context.Background(), LevelWarn, fmt.Sprintf(format, args...))
}

func (l *Logger) Warn(msg string, args ...any) {
	l.log(context.Background(), LevelWarn, msg, args...)
}

func (l *Logger) WarnWithContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelWarn, msg, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.log(context.Background(), LevelError, fmt.Sprintf(format, args...))
}

func (l *Logger) Error(msg string, args ...any) {
	l.log(context.Background(), LevelError, msg, args...)
}

func (l *Logger) ErrorWithContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelError, msg, args...)
}

func (l *Logger) Fatalf(format string, args ...any) {
	l.log(context.Background(), LevelError, fmt.Sprintf(format, args...))
	_ = Close()
	os.Exit(1)
}

func (l *Logger) Fatal(msg string, args ...any) {
	l.log(context.Background(), LevelError, msg, args...)
	_ = Close()
	os.Exit(1)
}

func (l *Logger) FatalWithContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelError, msg, args...)
	_ = Close()
	os.Exit(1)
}

func WithFields(f Fields) *Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger.WithFields(f)
}

func WithField(k string, v any) *Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger.WithField(k, v)
}

func WithError(err error) *Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger.WithError(err)
}

func Tracef(format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Tracef(format, args...)
}
func Trace(msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Trace(msg, args...)
}
func TraceWithContext(ctx context.Context, msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.TraceWithContext(ctx, msg, args...)
}
func Debugf(format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Debugf(format, args...)
}
func Debug(msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Debug(msg, args...)
}
func DebugWithContext(ctx context.Context, msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.DebugWithContext(ctx, msg, args...)
}
func Infof(format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Infof(format, args...)
}
func Info(msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Info(msg, args...)
}
func InfoWithContext(ctx context.Context, msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.InfoWithContext(ctx, msg, args...)
}
func Warnf(format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Warnf(format, args...)
}
func Warn(msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Warn(msg, args...)
}
func WarnWithContext(ctx context.Context, msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.WarnWithContext(ctx, msg, args...)
}
func Errorf(format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Errorf(format, args...)
}
func Error(msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Error(msg, args...)
}
func ErrorWithContext(ctx context.Context, msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.ErrorWithContext(ctx, msg, args...)
}
func Fatalf(format string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Fatalf(format, args...)
}
func Fatal(msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.Fatal(msg, args...)
}
func FatalWithContext(ctx context.Context, msg string, args ...any) {
	mu.RLock()
	defer mu.RUnlock()
	defaultLogger.FatalWithContext(ctx, msg, args...)
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
	var errs []error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			if err := handler.Handle(ctx, r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
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
