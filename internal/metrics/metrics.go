package metrics

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

type Counter struct {
	v atomic.Int64
}

func (c *Counter) Add(n int64) {
	c.v.Add(n)
}

func (c *Counter) Set(n int64) {
	c.v.Store(n)
}

func (c *Counter) Load() int64 {
	return c.v.Load()
}

type Labeled struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (l *Labeled) Inc(label string) {
	l.mu.Lock()
	if l.counts == nil {
		l.counts = map[string]int64{}
	}
	l.counts[label]++
	l.mu.Unlock()
}

func (l *Labeled) snapshot() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.counts))
	for k, v := range l.counts {
		out[k] = v
	}
	return out
}

var (
	Events     Labeled
	Exceptions Labeled
	VoiceClose Labeled
	Prefetch   Labeled
	AudioCache Labeled
	Seek       Labeled

	AudioCacheBytes Counter
	AudioCacheFiles Counter
	StallMs         Counter
)

type Gauge struct {
	Name  string
	Help  string
	Value float64
}

func Write(w io.Writer, gauges []Gauge) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	gauges = append(gauges,
		Gauge{"cadence_goroutines", "Live goroutines.", float64(runtime.NumGoroutine())},
		Gauge{"cadence_heap_bytes", "Heap in use.", float64(mem.HeapInuse)},
		Gauge{"cadence_gc_pause_total_seconds", "Cumulative GC pause.", float64(mem.PauseTotalNs) / 1e9},
		Gauge{"cadence_send_stall_ms_total", "Milliseconds the send loop fell behind its 20ms schedule.", float64(StallMs.Load())},
		Gauge{"cadence_audio_cache_bytes", "Bytes of audio held in the disk cache.", float64(AudioCacheBytes.Load())},
		Gauge{"cadence_audio_cache_files", "Tracks held in the disk cache.", float64(AudioCacheFiles.Load())},
	)
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.Name, g.Help, g.Name, g.Name, g.Value)
	}
	labeled(w, "cadence_events_total", "Player events emitted.", "type", &Events)
	labeled(w, "cadence_track_exceptions_total", "Track failures by source and severity.", "kind", &Exceptions)
	labeled(w, "cadence_voice_closes_total", "Voice gateway closes by code.", "code", &VoiceClose)
	labeled(w, "cadence_prefetch_total", "Track-start prefetch outcomes.", "outcome", &Prefetch)
	labeled(w, "cadence_audio_cache_total", "Disk audio cache outcomes.", "outcome", &AudioCache)
	labeled(w, "cadence_seek_total", "How seeks were served.", "outcome", &Seek)
}

func labeled(w io.Writer, name, help, label string, l *Labeled) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	snap := l.snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, snap[k])
	}
}
