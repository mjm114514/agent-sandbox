package log

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type Level string

const (
	Debug Level = "debug"
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

type Entry struct {
	Level Level  `json:"level"`
	Msg   string `json:"msg"`
	Ts    int64  `json:"ts"`
}

type Notifier func(method string, params any) error

type Logger struct {
	mu       sync.Mutex
	file     *os.File
	notifier Notifier
	minLevel Level
	subLevel Level
}

var levelOrder = map[Level]int{Debug: 0, Info: 1, Warn: 2, Error: 3}

func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &Logger{file: f, minLevel: Debug, subLevel: "none"}, nil
}

func (l *Logger) SetNotifier(n Notifier) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notifier = n
}

func (l *Logger) Subscribe(minLevel Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subLevel = minLevel
}

func (l *Logger) log(level Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	entry := Entry{Level: level, Msg: msg, Ts: time.Now().Unix()}

	l.mu.Lock()
	defer l.mu.Unlock()

	line, _ := json.Marshal(entry)
	l.file.Write(append(line, '\n'))

	if l.notifier != nil && l.subLevel != "none" && levelOrder[level] >= levelOrder[l.subLevel] {
		l.notifier("log", entry)
	}
}

func (l *Logger) Debug(format string, args ...any) { l.log(Debug, format, args...) }
func (l *Logger) Info(format string, args ...any)   { l.log(Info, format, args...) }
func (l *Logger) Warn(format string, args ...any)   { l.log(Warn, format, args...) }
func (l *Logger) Error(format string, args ...any)  { l.log(Error, format, args...) }

func (l *Logger) Close() {
	l.file.Close()
}
