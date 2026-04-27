package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

// RequestEvent is a single proxied request, surfaced to the dashboard.
type RequestEvent struct {
	Time      time.Time
	Hash      string
	Method    string
	Namespace string
	Path      string
	Status    int
	Hit       bool
	Mode      string
	DurMicros int64
}

// Stats keeps lock-free counters and a small ring buffer of recent events.
type Stats struct {
	hits   atomic.Int64
	misses atomic.Int64
	bypass atomic.Int64
	total  atomic.Int64

	mu     sync.Mutex
	recent []RequestEvent
	cap    int
}

func NewStats(capacity int) *Stats {
	if capacity <= 0 {
		capacity = 100
	}
	return &Stats{cap: capacity, recent: make([]RequestEvent, 0, capacity)}
}

func (s *Stats) Record(ev RequestEvent) {
	s.total.Add(1)
	switch {
	case ev.Hit:
		s.hits.Add(1)
	case ev.Mode == string(ModePassthrough):
		s.bypass.Add(1)
	default:
		s.misses.Add(1)
	}
	s.mu.Lock()
	if len(s.recent) >= s.cap {
		copy(s.recent, s.recent[1:])
		s.recent = s.recent[:s.cap-1]
	}
	s.recent = append(s.recent, ev)
	s.mu.Unlock()
}

// Snapshot is a point-in-time view safe to render.
type Snapshot struct {
	Hits   int64
	Misses int64
	Bypass int64
	Total  int64
	HitRatePct float64
	Recent []RequestEvent
}

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	recent := make([]RequestEvent, len(s.recent))
	copy(recent, s.recent)
	s.mu.Unlock()

	// newest first
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}

	hits := s.hits.Load()
	total := s.total.Load()
	rate := 0.0
	if total > 0 {
		rate = float64(hits) / float64(total) * 100
	}
	return Snapshot{
		Hits:       hits,
		Misses:     s.misses.Load(),
		Bypass:     s.bypass.Load(),
		Total:      total,
		HitRatePct: rate,
		Recent:     recent,
	}
}
