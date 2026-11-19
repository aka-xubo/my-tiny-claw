package tools

import "sync"

const defaultTaskLogLimit = 64 * 1024

type taskLog struct {
	mu    sync.Mutex
	data  []byte
	base  int64
	limit int
}

func newTaskLog(limit int) *taskLog {
	if limit <= 0 {
		limit = defaultTaskLogLimit
	}
	return &taskLog{limit: limit}
}

func (l *taskLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= l.limit {
		l.base += int64(len(l.data) + len(p) - l.limit)
		l.data = append(l.data[:0], p[len(p)-l.limit:]...)
		return len(p), nil
	}
	if overflow := len(l.data) + len(p) - l.limit; overflow > 0 {
		l.base += int64(overflow)
		copy(l.data, l.data[overflow:])
		l.data = l.data[:len(l.data)-overflow]
	}
	l.data = append(l.data, p...)
	return len(p), nil
}

func (l *taskLog) read(cursor int64, max int) (output string, next int64, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if max <= 0 {
		max = 8000
	}
	if cursor < l.base {
		cursor = l.base
		truncated = true
	}
	end := l.base + int64(len(l.data))
	if cursor > end {
		cursor = end
	}
	offset := int(cursor - l.base)
	available := l.data[offset:]
	if len(available) > max {
		available = available[:max]
		truncated = true
	}
	next = cursor + int64(len(available))
	return string(available), next, truncated
}

// tail prioritizes the final diagnostics, including output lost to eviction.
func (l *taskLog) tail(max int) (output string, next int64, truncated bool) {
	return l.tailFrom(0, max)
}

// tailFrom returns the tail of the unread range without replaying consumed
// bytes. Reaching the end consumes that range, including any skipped bytes.
func (l *taskLog) tailFrom(cursor int64, max int) (output string, next int64, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if max <= 0 {
		max = 8000
	}
	if cursor < l.base {
		cursor = l.base
		truncated = true
	}
	end := l.base + int64(len(l.data))
	if cursor > end {
		cursor = end
	}
	data := l.data[int(cursor-l.base):]
	if len(data) > max {
		data = data[len(data)-max:]
		truncated = true
	}
	return string(data), end, truncated
}

func (l *taskLog) end() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.base + int64(len(l.data))
}
