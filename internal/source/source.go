package source

import (
	"context"
	"io"

	"github.com/greed/cadence/internal/codec"
)

type Source interface {
	Name() string
	SearchPrefix() string
	Search(ctx context.Context, query string) ([]Track, error)
	MatchesURL(rawURL string) bool
	LoadURL(ctx context.Context, rawURL string) (LoadResult, error)
	Track(ctx context.Context, identifier string) (Track, error)
}

type Recommender interface {
	Recommend(ctx context.Context, identifier string, limit int) ([]Track, error)
}

type ISRCLookup interface {
	ByISRC(ctx context.Context, isrc string) (Track, error)
}

type Stream struct {
	Body        io.ReadCloser
	Format      codec.Format
	StartSample int64
}

type Seekable interface {
	OpenAt(ctx context.Context, t Track, startMs int64) (Stream, error)
}

type Playable interface {
	Source
	Open(ctx context.Context, t Track) (Stream, error)
	Warm(ctx context.Context, t Track)
}
