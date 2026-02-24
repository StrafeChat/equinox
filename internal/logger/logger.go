package logger

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

type Level int

const (
	LevelOff Level = iota
	LevelError
	LevelWarn
	LevelInfo
	LevelDebug
)

func (l Level) String() string {
	switch l {
	case LevelOff:
		return "off"
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	default:
		return "info"
	}
}

func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "silent", "none":
		return LevelOff
	case "error":
		return LevelError
	case "warn", "warning":
		return LevelWarn
	case "info":
		return LevelInfo
	case "debug":
		return LevelDebug
	default:
		return LevelInfo
	}
}

// Logger provides unified, level-filtered logging. Use for development to trace
// requests and debug issues; set LevelOff (or LOG_LEVEL=off) in production.
type Logger struct {
	level  Level
	prefix string
}

// New creates a Logger. prefix is prepended to all messages (e.g. "equinox").
func New(level Level, prefix string) *Logger {
	return &Logger{level: level, prefix: prefix}
}

func (l *Logger) enabled(min Level) bool {
	return l.level >= min && l.level != LevelOff
}

func (l *Logger) log(min Level, module, format string, args ...any) {
	if !l.enabled(min) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	tag := "[" + l.prefix
	if module != "" {
		tag = tag + "][" + module
	}
	tag += "]"
	log.Output(2, tag+" "+msg)
}

func (l *Logger) Debug(module, format string, args ...any) { l.log(LevelDebug, module, format, args...) }
func (l *Logger) Info(module, format string, args ...any)    { l.log(LevelInfo, module, format, args...) }
func (l *Logger) Warn(module, format string, args ...any)   { l.log(LevelWarn, module, format, args...) }
func (l *Logger) Error(module, format string, args ...any) { l.log(LevelError, module, format, args...) }

func (l *Logger) Request(method, path string, status int, latency time.Duration) {
	if !l.enabled(LevelInfo) {
		return
	}
	l.log(LevelInfo, "http", "%s %s %d %s", method, path, status, latency.Round(time.Millisecond))
}

func (l *Logger) Err(module string, err error, ctx map[string]any) {
	if !l.enabled(LevelError) || err == nil {
		return
	}
	msg := fmt.Sprintf("error: %v", err)
	if len(ctx) > 0 {
		var pairs []string
		for k, v := range ctx {
			pairs = append(pairs, fmt.Sprintf("%s=%v", k, v))
		}
		msg += " " + strings.Join(pairs, " ")
	}
	l.log(LevelError, module, "%s", msg)
}

var defaultLogger *Logger

func InitDefault(level Level) {
	defaultLogger = New(level, "equinox")
}

func InitFromEnv() {
	level := ParseLevel(os.Getenv("LOG_LEVEL"))
	InitDefault(level)
}

func Debug(module, format string, args ...any) {
	if defaultLogger != nil {
		defaultLogger.Debug(module, format, args...)
	}
}
func Info(module, format string, args ...any) {
	if defaultLogger != nil {
		defaultLogger.Info(module, format, args...)
	}
}
func Warn(module, format string, args ...any) {
	if defaultLogger != nil {
		defaultLogger.Warn(module, format, args...)
	}
}
func Error(module, format string, args ...any) {
	if defaultLogger != nil {
		defaultLogger.Error(module, format, args...)
	}
}
func Request(method, path string, status int, latency time.Duration) {
	if defaultLogger != nil {
		defaultLogger.Request(method, path, status, latency)
	}
}
func Err(module string, err error, context map[string]any) {
	if defaultLogger != nil {
		defaultLogger.Err(module, err, context)
	}
}
