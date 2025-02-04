package mpquic

import (
	"log"
	"os"
)

type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

var (
	currentLogLevel = LogLevelInfo
)

type Logger struct {
	level LogLevel
}

func SetLogLevel(level LogLevel) {
	currentLogLevel = level
}

func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

func logf(level LogLevel, format string, v ...interface{}) {
	if level >= currentLogLevel {
		log.Printf("["+level.String()+"] "+format, v...)
	}
}

func Debug(format string, v ...interface{}) {
	logf(LogLevelDebug, format, v...)
}

func Info(format string, v ...interface{}) {
	logf(LogLevelInfo, format, v...)
}

func Warn(format string, v ...interface{}) {
	logf(LogLevelWarn, format, v...)
}

func Error(format string, v ...interface{}) {
	logf(LogLevelError, format, v...)
}

func SetLogLevelString(level string) {
	switch level {
	case "DEBUG":
		SetLogLevel(LogLevelDebug)
	case "INFO":
		SetLogLevel(LogLevelInfo)
	case "WARN":
		SetLogLevel(LogLevelWarn)
	case "ERROR":
		SetLogLevel(LogLevelError)
	default:
		SetLogLevel(LogLevelInfo)
	}
}

func init() {
	// 检查环境变量 DEBUG
	if os.Getenv("DEBUG") == "1" {
		SetLogLevel(LogLevelDebug)
	}
}
