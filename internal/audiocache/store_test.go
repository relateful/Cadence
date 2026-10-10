package audiocache

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func put(t *testing.T, s *Store, key string, data []byte) {
	t.Helper()
	w, err := s.Create(key, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, s *Store, key string) ([]byte, bool) {
	t.Helper()
	f, _, ok := s.Get(key)
	if !ok {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

func TestCommitThenGetReturnsTheBytes(t *testing.T) {
	s, err := Open(t.TempDir(), 1<<20, quiet())
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "123-mp3_128", []byte("hello"))
	if got, ok := read(t, s, "123-mp3_128"); !ok || string(got) != "hello" {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := read(t, s, "999-mp3_128"); ok {
		t.Fatal("missing key was found")
	}
}

func TestAbortedAndIncompleteWritesLeaveNothing(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 1<<20, quiet())
	w, _ := s.Create("a", 0)
	w.Write([]byte("partial"))
	w.Abort()
	short, _ := s.Create("b", 100)
	short.Write([]byte("too short"))
	if err := short.Commit(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("short commit: %v", err)
	}
	if s.Has("a") || s.Has("b") {
		t.Fatal("unfinished write was cached")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("files left behind: %v", entries)
	}
}

func TestEvictsLeastRecentlyPlayedPastTheLimit(t *testing.T) {
	s, _ := Open(t.TempDir(), 25, quiet())
	put(t, s, "old", bytes.Repeat([]byte("o"), 10))
	put(t, s, "mid", bytes.Repeat([]byte("m"), 10))
	read(t, s, "old")
	put(t, s, "new", bytes.Repeat([]byte("n"), 10))
	if s.Has("mid") {
		t.Fatal("least recently played entry survived")
	}
	if !s.Has("old") || !s.Has("new") {
		t.Fatal("recently played entry was evicted")
	}
	if _, used, _ := s.Stats(); used != 20 {
		t.Fatalf("used %d", used)
	}
}

func TestReopenRestoresIndexAndClearsTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 1<<20, quiet())
	put(t, s, "older", []byte("1"))
	put(t, s, "newer", []byte("22"))
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "older"+fileExt), old, old)
	os.WriteFile(filepath.Join(dir, "x-123"+tempExt), []byte("junk"), 0o600)
	os.WriteFile(filepath.Join(dir, "not a key!"+fileExt), []byte("junk"), 0o600)

	s2, err := Open(dir, 2, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has("newer") || s2.Has("older") {
		t.Fatal("reopen should keep the newest file within the limit")
	}
	if _, err := os.Stat(filepath.Join(dir, "x-123"+tempExt)); !os.IsNotExist(err) {
		t.Fatal("leftover temp file not removed")
	}
	if files, used, _ := s2.Stats(); files != 1 || used != 2 {
		t.Fatalf("files %d used %d", files, used)
	}
}

func TestRejectsBadKeysAndOversizedFiles(t *testing.T) {
	s, _ := Open(t.TempDir(), 1<<20, quiet())
	if _, err := s.Create("../escape", 0); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := s.Create("big", MaxFileSize+1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	if _, _, ok := s.Get("../escape"); ok {
		t.Fatal("bad key read")
	}
}

func TestFileChangedOnDiskIsDropped(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 1<<20, quiet())
	put(t, s, "k", []byte("abc"))
	os.WriteFile(filepath.Join(dir, "k"+fileExt), []byte("abcdef"), 0o600)
	if _, ok := read(t, s, "k"); ok {
		t.Fatal("resized file was served")
	}
	if s.Has("k") {
		t.Fatal("resized file kept in index")
	}
}
