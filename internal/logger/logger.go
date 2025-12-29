package logger

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	level  Level
	fields map[string]interface{}
}

func New() *Logger {
	return &Logger{
		out:    os.Stdout,
		level:  LevelInfo,
		fields: make(map[string]interface{}),
	}
}

func (l *Logger) SetLevel(level Level) {
	l.level = level
}

func (l *Logger) With(keyvals ...interface{}) *Logger {
	newLogger := &Logger{
		out:    l.out,
		level:  l.level,
		fields: make(map[string]interface{}),
	}
	for k, v := range l.fields {
		newLogger.fields[k] = v
	}
	for i := 0; i < len(keyvals)-1; i += 2 {
		if key, ok := keyvals[i].(string); ok {
			newLogger.fields[key] = keyvals[i+1]
		}
	}
	return newLogger
}

func (l *Logger) log(level Level, msg string, keyvals ...interface{}) {
	if level < l.level {
		return
	}

	entry := make(map[string]interface{})
	entry["time"] = time.Now().UTC().Format(time.RFC3339)
	entry["level"] = level.String()
	entry["msg"] = msg

	for k, v := range l.fields {
		entry[k] = v
	}

	for i := 0; i < len(keyvals)-1; i += 2 {
		if key, ok := keyvals[i].(string); ok {
			entry[key] = keyvals[i+1]
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	data, _ := json.Marshal(entry)
	l.out.Write(data)
	l.out.Write([]byte("\n"))
}

func (l *Logger) Debug(msg string, keyvals ...interface{}) {
	l.log(LevelDebug, msg, keyvals...)
}

func (l *Logger) Info(msg string, keyvals ...interface{}) {
	l.log(LevelInfo, msg, keyvals...)
}

func (l *Logger) Warn(msg string, keyvals ...interface{}) {
	l.log(LevelWarn, msg, keyvals...)
}

func (l *Logger) Error(msg string, keyvals ...interface{}) {
	l.log(LevelError, msg, keyvals...)
}
