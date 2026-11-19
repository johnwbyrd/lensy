package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Log is an append-only JSONL file. Each Append is fsynced before it returns,
// so a committed event survives a crash. This is the "commit before notify"
// boundary: nothing is sent to an operator until the record is durable.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	seq  uint64
	path string
}

func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, w: bufio.NewWriter(f), path: path}, nil
}

// Replay feeds every stored event, in order, to fn. It is how state is rebuilt
// at startup and how a release can be checked against history.
func (l *Log) Replay(fn func(Event)) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Seek(0, 0); err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(l.f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return n, fmt.Errorf("line %d: %w", n+1, err)
		}
		if ev.Seq > l.seq {
			l.seq = ev.Seq
		}
		fn(ev)
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	_, err := l.f.Seek(0, 2)
	return n, err
}

// Append assigns the next sequence number, writes and fsyncs.
func (l *Log) Append(ev *Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ev.Seq = l.seq
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := l.w.Write(b); err != nil {
		return err
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *Log) Close() error { return l.f.Close() }
