package audiocache

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/metrics"
)

const (
	fileExt     = ".audio"
	tempExt     = ".tmp"
	MaxFileSize = 64 << 20
)

var (
	keyPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)
	ErrInvalidKey  = errors.New("audio cache key must be 1-96 characters of [A-Za-z0-9_-]")
	ErrTooLarge    = fmt.Errorf("audio file exceeds the %d MiB cache limit", MaxFileSize>>20)
	ErrIncomplete  = errors.New("audio download did not complete")
	errWriterSpent = errors.New("cache writer already committed or aborted")
)

type entry struct {
	key  string
	size int64
}

type Store struct {
	dir   string
	limit int64
	log   *slog.Logger

	mu    sync.Mutex
	used  int64
	order *list.List
	items map[string]*list.Element
}

func Open(dir string, limit int64, log *slog.Logger) (*Store, error) {
	if limit <= 0 {
		return nil, errors.New("audio cache limit must be positive")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audio cache dir: %w", err)
	}
	s := &Store{dir: dir, limit: limit, log: log.With("component", "audiocache"), order: list.New(), items: map[string]*list.Element{}}
	if err := s.scan(); err != nil {
		return nil, err
	}
	s.evictLocked()
	s.publishLocked()
	s.log.Info("audio cache ready", "dir", dir, "files", s.order.Len(), "usedBytes", s.used, "limitBytes", limit)
	return s, nil
}

type found struct {
	key     string
	size    int64
	modTime time.Time
}

func (s *Store) scan() error {
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("audio cache scan: %w", err)
	}
	var files []found
	for _, d := range dirents {
		name := d.Name()
		path := filepath.Join(s.dir, name)
		if strings.HasSuffix(name, tempExt) {
			if err := os.Remove(path); err != nil {
				s.log.Warn("could not remove leftover temp file", "file", name, "error", err)
			}
			continue
		}
		key, ok := strings.CutSuffix(name, fileExt)
		if !ok || !keyPattern.MatchString(key) || d.IsDir() {
			continue
		}
		info, err := d.Info()
		if err != nil {
			s.log.Warn("could not stat cached file", "file", name, "error", err)
			continue
		}
		files = append(files, found{key: key, size: info.Size(), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	for _, f := range files {
		s.items[f.key] = s.order.PushBack(&entry{key: f.key, size: f.size})
		s.used += f.size
	}
	return nil
}

func (s *Store) path(key string) string {
	return filepath.Join(s.dir, key+fileExt)
}

func (s *Store) Get(key string) (*os.File, int64, bool) {
	if !keyPattern.MatchString(key) {
		return nil, 0, false
	}
	s.mu.Lock()
	el, ok := s.items[key]
	var size int64
	if ok {
		s.order.MoveToFront(el)
		size = el.Value.(*entry).size
	}
	s.mu.Unlock()
	if !ok {
		metrics.AudioCache.Inc("miss")
		return nil, 0, false
	}
	f, err := os.Open(s.path(key))
	if err != nil {
		s.log.Warn("cached file unreadable, dropping it", "key", key, "error", err)
		s.drop(key)
		metrics.AudioCache.Inc("read_failed")
		return nil, 0, false
	}
	info, err := f.Stat()
	if err != nil || info.Size() != size {
		f.Close()
		s.log.Warn("cached file changed size, dropping it", "key", key, "error", err)
		s.drop(key)
		metrics.AudioCache.Inc("read_failed")
		return nil, 0, false
	}
	now := time.Now()
	if err := os.Chtimes(s.path(key), now, now); err != nil {
		s.log.Debug("could not touch cached file", "key", key, "error", err)
	}
	metrics.AudioCache.Inc("hit")
	return f, info.Size(), true
}

func (s *Store) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.items[key]
	return ok
}

func (s *Store) drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.removeLocked(el)
		s.publishLocked()
	}
}

func (s *Store) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	s.order.Remove(el)
	delete(s.items, e.key)
	s.used -= e.size
	if err := os.Remove(s.path(e.key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("could not delete cached file", "key", e.key, "error", err)
	}
}

func (s *Store) evictLocked() {
	for s.used > s.limit && s.order.Len() > 0 {
		s.removeLocked(s.order.Back())
		metrics.AudioCache.Inc("evicted")
	}
}

func (s *Store) publishLocked() {
	metrics.AudioCacheBytes.Set(s.used)
	metrics.AudioCacheFiles.Set(int64(s.order.Len()))
}

type Writer struct {
	store    *Store
	key      string
	file     *os.File
	written  int64
	expected int64
	err      error
	done     bool
}

func (s *Store) Create(key string, expected int64) (*Writer, error) {
	if !keyPattern.MatchString(key) {
		return nil, ErrInvalidKey
	}
	if expected > MaxFileSize {
		return nil, ErrTooLarge
	}
	f, err := os.CreateTemp(s.dir, key+"-*"+tempExt)
	if err != nil {
		return nil, fmt.Errorf("audio cache temp file: %w", err)
	}
	return &Writer{store: s, key: key, file: f, expected: expected}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.written+int64(len(p)) > MaxFileSize {
		w.err = ErrTooLarge
		return 0, w.err
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *Writer) Commit() error {
	if w.done {
		return errWriterSpent
	}
	w.done = true
	tmp := w.file.Name()
	closeErr := w.file.Close()
	switch {
	case w.err != nil:
		os.Remove(tmp)
		return w.err
	case closeErr != nil:
		os.Remove(tmp)
		return closeErr
	case w.written == 0, w.expected > 0 && w.written != w.expected:
		os.Remove(tmp)
		return fmt.Errorf("%w: %d of %d bytes", ErrIncomplete, w.written, w.expected)
	}
	s := w.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tmp, s.path(w.key)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("audio cache commit: %w", err)
	}
	if el, ok := s.items[w.key]; ok {
		e := el.Value.(*entry)
		s.used += w.written - e.size
		e.size = w.written
		s.order.MoveToFront(el)
	} else {
		s.items[w.key] = s.order.PushFront(&entry{key: w.key, size: w.written})
		s.used += w.written
	}
	s.evictLocked()
	s.publishLocked()
	metrics.AudioCache.Inc("stored")
	return nil
}

func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.file.Close()
	os.Remove(w.file.Name())
}

func (s *Store) Stats() (files int, used, limit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len(), s.used, s.limit
}

var _ io.Writer = (*Writer)(nil)
