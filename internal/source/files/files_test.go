package files

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source"
)

const frameSize = 417

func textFrame(id string, enc byte, body []byte) []byte {
	out := []byte(id)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)+1))
	out = append(out, 0, 0, enc)
	return append(out, body...)
}

func id3(frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	n := len(body)
	return append([]byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}, body...)
}

func utf16le(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

func mp3(tag []byte, seconds int) []byte {
	out := append([]byte(nil), tag...)
	frames := seconds * 128_000 / 8 / frameSize
	for range frames {
		f := make([]byte, frameSize)
		copy(f, []byte{0xFF, 0xFB, 0x90, 0x64})
		out = append(out, f...)
	}
	return out
}

func taggedSong() []byte {
	return mp3(id3(textFrame("TIT2", 0, []byte("Midnight City")), textFrame("TPE1", 1, utf16le("M83")), textFrame("TALB", 3, []byte("Hurry Up, We're Dreaming"))), 30)
}

func TestReadTagsDecodesEveryTextEncoding(t *testing.T) {
	got := readTags(taggedSong())
	if got.title != "Midnight City" || got.artist != "M83" || got.album != "Hurry Up, We're Dreaming" {
		t.Fatalf("%+v", got)
	}
	if got := readTags([]byte{0xFF, 0xFB, 0x90, 0x64}); got != (tags{}) {
		t.Fatalf("read tags from a bare frame: %+v", got)
	}
}

func TestReadTagsStopsAtATruncatedFrame(t *testing.T) {
	tag := id3(textFrame("TIT2", 0, []byte("Whole")), textFrame("TPE1", 0, []byte("Cut")))
	got := readTags(tag[:len(tag)-2])
	if got.title != "Whole" || got.artist != "" {
		t.Fatalf("%+v", got)
	}
}

func musicDir(t *testing.T) (*Local, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "albums"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("albums/midnight city.mp3", taggedSong())
	write("untagged song.mp3", mp3(nil, 10))
	write("notes.txt", []byte("not audio"))
	write("fake.mp3", []byte("<html>not audio</html>"))
	l, err := NewLocal(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, dir
}

func TestLocalLoadsATaggedFile(t *testing.T) {
	l, dir := musicDir(t)
	res, err := l.LoadURL(context.Background(), "local:albums/midnight city.mp3")
	if err != nil || res.Type != source.LoadTrack {
		t.Fatalf("%+v %v", res, err)
	}
	got := res.Tracks[0]
	if got.Source != LocalName || got.Identifier != "albums/midnight city.mp3" || got.Title != "Midnight City" || got.Author != "M83" || got.IsStream {
		t.Fatalf("%+v", got)
	}
	if got.DurationMs < 29_000 || got.DurationMs > 31_000 {
		t.Fatalf("duration %d", got.DurationMs)
	}
	abs := filepath.ToSlash(filepath.Join(dir, "albums", "midnight city.mp3"))
	fileURL := (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(abs, "/")}).String()
	for _, form := range []string{"local:" + filepath.Join(dir, "albums", "midnight city.mp3"), fileURL} {
		if res, err := l.LoadURL(context.Background(), form); err != nil || res.Tracks[0].Identifier != got.Identifier {
			t.Fatalf("%s: %+v %v", form, res, err)
		}
	}
}

func TestLocalFallsBackToTheFileName(t *testing.T) {
	l, _ := musicDir(t)
	got, err := l.Track(context.Background(), "untagged song.mp3")
	if err != nil || got.Title != "untagged song" || got.Author != unknownArtist {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLocalRefusesPathsOutsideTheDirectory(t *testing.T) {
	l, dir := musicDir(t)
	outside := filepath.Join(filepath.Dir(dir), "secret.mp3")
	if err := os.WriteFile(outside, mp3(nil, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	for _, raw := range []string{"local:../secret.mp3", "local:albums/../../secret.mp3", "local:" + outside} {
		if _, err := l.LoadURL(context.Background(), raw); err == nil {
			t.Fatalf("%s loaded", raw)
		}
	}
	if _, err := l.Open(context.Background(), source.Track{Source: LocalName, Identifier: "../secret.mp3"}); err == nil {
		t.Fatal("a forged track opened a file outside the directory")
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.mp3")); err == nil {
		if _, err := l.Open(context.Background(), source.Track{Source: LocalName, Identifier: "link.mp3"}); err == nil {
			t.Fatal("a symlink escaped the directory")
		}
	}
}

func TestLocalRefusesFilesItCannotPlay(t *testing.T) {
	l, _ := musicDir(t)
	for _, id := range []string{"notes.txt", "fake.mp3", "missing.mp3", "albums"} {
		if _, err := l.Track(context.Background(), id); err == nil {
			t.Fatalf("%s loaded", id)
		}
	}
}

func TestLocalSearchMatchesEveryWordOfThePath(t *testing.T) {
	l, _ := musicDir(t)
	got, err := l.Search(context.Background(), "MIDNIGHT albums")
	if err != nil || len(got) != 1 || got[0].Title != "Midnight City" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ := l.Search(context.Background(), "notes"); len(got) != 0 {
		t.Fatalf("matched a non-mp3 file: %+v", got)
	}
}

func TestLocalOpenStreamsTheFile(t *testing.T) {
	l, _ := musicDir(t)
	s, err := l.Open(context.Background(), source.Track{Source: LocalName, Identifier: "albums/midnight city.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Body.Close()
	body, _ := io.ReadAll(s.Body)
	if !bytes.Equal(body, taggedSong()) {
		t.Fatal("streamed bytes differ from the file")
	}
}

func testClient() *httpx.Client {
	c, _ := httpx.New("test", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c
}

func TestRemoteMatchesOnlyAllowedHosts(t *testing.T) {
	r := NewRemote(testClient(), []string{"Example.com", " cdn.discordapp.com. "})
	for raw, want := range map[string]bool{
		"https://example.com/a.mp3":          true,
		"https://media.example.com/a.mp3":    true,
		"http://cdn.discordapp.com/x/a.mp3":  true,
		"https://notexample.com/a.mp3":       false,
		"https://example.com.evil.net/a.mp3": false,
		"ftp://example.com/a.mp3":            false,
		"https://user:pw@example.com/a.mp3":  false,
		"local:a.mp3":                        false,
	} {
		if got := r.MatchesURL(raw); got != want {
			t.Fatalf("%s: got %v", raw, got)
		}
	}
	if !NewRemote(testClient(), []string{AnyHost}).MatchesURL("https://anything.net/a.mp3") {
		t.Fatal("the wildcard did not match")
	}
}

func audioServer(t *testing.T, ranges bool, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		if ranges {
			http.ServeContent(w, r, "a.mp3", time.Time{}, bytes.NewReader(data))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	}))
}

func TestRemoteLoadsWithAndWithoutRangeSupport(t *testing.T) {
	for _, ranges := range []bool{true, false} {
		srv := audioServer(t, ranges, taggedSong())
		r := NewRemote(testClient(), []string{"127.0.0.1"})
		got, err := r.Track(context.Background(), srv.URL+"/music/midnight%20city.mp3")
		srv.Close()
		if err != nil || got.Title != "Midnight City" || got.Author != "M83" || got.IsStream || got.DurationMs < 29_000 || got.DurationMs > 31_000 {
			t.Fatalf("ranges=%v: %+v %v", ranges, got, err)
		}
	}
}

func TestRemoteNamesUntaggedFilesFromThePath(t *testing.T) {
	srv := audioServer(t, true, mp3(nil, 5))
	defer srv.Close()
	got, err := NewRemote(testClient(), []string{"127.0.0.1"}).Track(context.Background(), srv.URL+"/a/my%20song.mp3?sig=1")
	if err != nil || got.Title != "my song" || got.Author != unknownArtist || got.Identifier != srv.URL+"/a/my%20song.mp3?sig=1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRemoteRefusesPagesAndForgedTracks(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html></html>"))
	}))
	defer page.Close()
	r := NewRemote(testClient(), []string{"127.0.0.1"})
	if _, err := r.Track(context.Background(), page.URL+"/a.mp3"); err == nil {
		t.Fatal("an html page loaded as a track")
	}
	if _, err := r.Open(context.Background(), source.Track{Source: RemoteName, Identifier: "https://not-allowed.net/a.mp3"}); err == nil {
		t.Fatal("a forged track opened a host outside the allowlist")
	}
}

func TestRemoteNeverReachesPrivateAddresses(t *testing.T) {
	srv := audioServer(t, true, taggedSong())
	defer srv.Close()
	r := NewRemote(httpx.NewPublic(RemoteName, slog.New(slog.NewTextHandler(io.Discard, nil)), func(string) bool { return true }), []string{AnyHost})
	if _, err := r.Track(context.Background(), srv.URL+"/a.mp3"); err == nil {
		t.Fatal("the public client fetched a loopback address")
	}
}

func TestReadTagsHandlesSyncsafeV24Sizes(t *testing.T) {
	title := bytes.Repeat([]byte("a"), 200)
	frame := append([]byte("TIT2"), 0, 0, 1, 73, 0, 0, 3)
	frame = append(frame, title...)
	n := len(frame)
	tag := append([]byte{'I', 'D', '3', 4, 0, 0, byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}, frame...)
	if got := readTags(tag); got.title != string(title) {
		t.Fatalf("v2.4 title %q", got.title)
	}
}

func TestLocalAcceptsLocalhostFileURLs(t *testing.T) {
	l, dir := musicDir(t)
	abs := filepath.ToSlash(filepath.Join(dir, "untagged song.mp3"))
	raw := (&url.URL{Scheme: "file", Host: "localhost", Path: "/" + strings.TrimPrefix(abs, "/")}).String()
	if _, err := l.LoadURL(context.Background(), raw); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	other := (&url.URL{Scheme: "file", Host: "fileserver", Path: "/" + strings.TrimPrefix(abs, "/")}).String()
	if _, err := l.LoadURL(context.Background(), other); err == nil {
		t.Fatal("a file url on another host loaded")
	}
}

func TestLocalResolvesPathsThroughASymlinkedDirectory(t *testing.T) {
	_, dir := musicDir(t)
	link := filepath.Join(t.TempDir(), "music")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("symlinks are not available here")
	}
	via, err := NewLocal(link, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer via.Close()
	if _, err := via.LoadURL(context.Background(), "local:"+filepath.Join(dir, "untagged song.mp3")); err != nil {
		t.Fatalf("the real path of a symlinked directory was refused: %v", err)
	}
}

type stubAudio struct{}

func (stubAudio) Name() string           { return "stub" }
func (stubAudio) SearchPrefix() string   { return "" }
func (stubAudio) MatchesURL(string) bool { return false }
func (stubAudio) LoadURL(context.Context, string) (source.LoadResult, error) {
	return source.Empty(), nil
}
func (stubAudio) Search(context.Context, string) ([]source.Track, error) { return nil, nil }
func (stubAudio) Track(context.Context, string) (source.Track, error)    { return source.Track{}, nil }
func (stubAudio) Open(context.Context, source.Track) (source.Stream, error) {
	return source.Stream{}, nil
}
func (stubAudio) Warm(context.Context, source.Track) {}

func TestResolverRoutesLocalIdentifiers(t *testing.T) {
	l, _ := musicDir(t)
	r := source.NewResolver(stubAudio{}, []string{"stub"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.UseDirect(l)
	if res := r.Load(context.Background(), "local:albums/midnight city.mp3"); res.Type != source.LoadTrack || res.Tracks[0].Source != LocalName {
		t.Fatalf("local: %+v", res)
	}
	if res := r.Load(context.Background(), "localsearch:midnight"); res.Type != source.LoadSearch || len(res.Tracks) != 1 {
		t.Fatalf("localsearch: %+v", res)
	}
	if res := r.Load(context.Background(), "local:../escape.mp3"); res.Type != source.LoadError {
		t.Fatalf("escape: %+v", res)
	}
}
