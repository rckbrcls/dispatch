package adapter

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"

	"dispatch/internal/sandbox"
)

// liveLog mirrors agent output into req.LogPath while the agent runs, so a
// viewer can follow it. Lines are redacted before they reach disk. Any I/O
// failure disables the mirror; it never fails the run.
type liveLog struct {
	mu   sync.Mutex
	f    *os.File
	line []byte
}

// openLiveLog returns the extra writers for stdout and stderr and a close
// func. With an empty path both writers discard.
func openLiveLog(path string) (stdout, stderr io.Writer, closeFn func()) {
	if path == "" {
		return io.Discard, io.Discard, func() {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return io.Discard, io.Discard, func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return io.Discard, io.Discard, func() {}
	}
	l := &liveLog{f: f}
	return l, l, l.close
}

func (l *liveLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	l.line = append(l.line, p...)
	for {
		i := bytes.IndexByte(l.line, '\n')
		if i < 0 {
			break
		}
		l.emit(l.line[:i+1])
		l.line = l.line[i+1:]
	}
	if len(l.line) > 1<<20 { // unterminated giant line: flush instead of growing
		l.emit(append(l.line, '\n'))
		l.line = nil
	}
	return len(p), nil
}

func (l *liveLog) emit(b []byte) {
	if _, err := l.f.WriteString(sandbox.Redact(string(b))); err != nil {
		l.f.Close()
		l.f = nil
	}
}

func (l *liveLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if len(l.line) > 0 {
		l.emit(append(l.line, '\n'))
		l.line = nil
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
