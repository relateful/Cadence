package files

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/source"
)

const (
	LocalName         = "local"
	LocalPrefix       = "local:"
	LocalSearchPrefix = "localsearch:"
	fileScheme        = "file://"
	maxScan           = 50_000
	maxResults        = 50
)

type Local struct {
	dir      string
	resolved string
	root     *os.Root
	log      *slog.Logger
}

func NewLocal(dir string, log *slog.Logger) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &Local{dir: abs, resolved: resolved, root: root, log: log.With("source", LocalName)}, nil
}

func (l *Local) inside(abs string) string {
	for _, base := range []string{l.dir, l.resolved} {
		if rel, err := filepath.Rel(base, abs); err == nil && filepath.IsLocal(rel) {
			return rel
		}
	}
	return abs
}

func (l *Local) Name() string         { return LocalName }
func (l *Local) SearchPrefix() string { return LocalSearchPrefix }

func (l *Local) MatchesURL(raw string) bool {
	return strings.HasPrefix(raw, LocalPrefix) || strings.HasPrefix(raw, fileScheme)
}

func (l *Local) fail(message string) error {
	return source.Fail(source.SeverityCommon, LocalName, message, nil)
}

func (l *Local) relative(raw string) (string, error) {
	p, ok := strings.CutPrefix(raw, LocalPrefix)
	if !ok {
		u, err := url.Parse(raw)
		if err != nil || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) {
			return "", l.fail("not a local file path")
		}
		p = u.Path
		if len(p) > 1 && filepath.VolumeName(p[1:]) != "" {
			p = p[1:]
		}
	}
	p = strings.TrimSpace(p)
	if filepath.IsAbs(p) {
		p = l.inside(p)
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", l.fail("path is outside the local music directory")
	}
	return p, nil
}

func (l *Local) LoadURL(ctx context.Context, raw string) (source.LoadResult, error) {
	rel, err := l.relative(raw)
	if err != nil {
		return source.LoadResult{}, err
	}
	t, err := l.Track(ctx, rel)
	if err != nil {
		return source.LoadResult{}, err
	}
	return source.Single(t), nil
}

func (l *Local) open(identifier string) (*os.File, fs.FileInfo, error) {
	rel, err := l.relative(LocalPrefix + identifier)
	if err != nil {
		return nil, nil, err
	}
	if !mp3Name(rel) {
		return nil, nil, l.fail("only mp3 files are supported")
	}
	f, err := l.root.Open(filepath.FromSlash(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, l.fail("file not found")
	}
	if err != nil {
		return nil, nil, source.Fail(source.SeverityCommon, LocalName, "file could not be opened", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, l.fail("not a regular file")
	}
	return f, info, nil
}

func (l *Local) Track(_ context.Context, identifier string) (source.Track, error) {
	f, info, err := l.open(identifier)
	if err != nil {
		return source.Track{}, err
	}
	defer f.Close()
	fetch := func(offset, n int64) ([]byte, error) {
		buf := make([]byte, n)
		read, err := f.ReadAt(buf, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return buf[:read], nil
	}
	p, err := probe(LocalName, fetch, info.Size())
	if err != nil {
		return source.Track{}, err
	}
	rel, _ := l.relative(LocalPrefix + identifier)
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(l.dir, filepath.FromSlash(rel)))}).String()
	return p.track(LocalName, rel, uri, path.Base(rel)), nil
}

func (l *Local) Search(ctx context.Context, query string) ([]source.Track, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, nil
	}
	var matches []string
	scanned := 0
	err := fs.WalkDir(l.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			l.log.Warn("local search skipped an unreadable path", "path", p, "error", err)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if scanned++; scanned > maxScan {
			l.log.Warn("local search stopped at the scan limit", "query", query, "limit", maxScan)
			return fs.SkipAll
		}
		if len(matches) >= maxResults {
			return fs.SkipAll
		}
		if d.IsDir() || !mp3Name(p) {
			return nil
		}
		name := strings.ToLower(p)
		for _, w := range words {
			if !strings.Contains(name, w) {
				return nil
			}
		}
		matches = append(matches, p)
		return nil
	})
	if err != nil {
		return nil, source.Fail(source.SeverityFault, LocalName, "the local music directory could not be searched", err)
	}
	out := make([]source.Track, 0, len(matches))
	for _, m := range matches {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		t, err := l.Track(ctx, m)
		if err != nil {
			l.log.Warn("local search skipped a file it cannot play", "path", m, "error", err)
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (l *Local) Open(_ context.Context, t source.Track) (source.Stream, error) {
	f, _, err := l.open(t.Identifier)
	if err != nil {
		return source.Stream{}, err
	}
	return source.Stream{Body: f, Format: codec.FormatMP3}, nil
}

func (l *Local) Warm(context.Context, source.Track) {}

func (l *Local) Close() error {
	return l.root.Close()
}
