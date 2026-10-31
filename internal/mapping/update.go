package mapping

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/ratelimit"
)

const DefaultAniBridgeURL = "https://github.com/anibridge/anibridge-mappings/releases/download/v3/mappings.min.json"
const DefaultAnimeListsURL = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-full.xml"

// Store holds immutable parsed snapshots. The parent can persist source bytes
// using its own state store and call Load after restart. A failed update leaves
// the previously installed snapshot in place.
type Store struct {
	mu                sync.RWMutex
	primary, fallback *Snapshot
	HTTP              *http.Client
}

func NewStore(client *http.Client) *Store {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Store{HTTP: client}
}
func (s *Store) HasSnapshots() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primary != nil || s.fallback != nil
}
func (s *Store) Resolver() *Resolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return NewResolver(s.primary, s.fallback)
}
func (s *Store) LoadAniBridge(data []byte) (Source, error) {
	parsed, e := ParseAniBridge(data)
	if e != nil {
		return Source{}, e
	}
	s.mu.Lock()
	s.primary = parsed
	s.mu.Unlock()
	return parsed.Source, nil
}
func (s *Store) LoadAnimeLists(data []byte) (Source, error) {
	parsed, e := ParseAnimeLists(data)
	if e != nil {
		return Source{}, e
	}
	s.mu.Lock()
	s.fallback = parsed
	s.mu.Unlock()
	return parsed.Source, nil
}
func (s *Store) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, e := url.Parse(rawURL)
	if e != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("mapping URL must use HTTPS")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	bounded := *client
	bounded.Timeout = 30 * time.Second
	bounded.Transport = ratelimit.Wrap(client.Transport, ratelimit.ExternalInterval)
	bounded.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || len(via) > 5 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, e := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("User-Agent", ratelimit.UserAgent)
		resp, e := bounded.Do(req)
		if e != nil {
			if requestCtx.Err() != nil {
				return nil, requestCtx.Err()
			}
			if errors.Is(e, ratelimit.ErrDeferred) {
				return nil, errors.New("mapping rate limit exceeds request budget; retry later")
			}
			return nil, e
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			resp.Body.Close()
			if attempt < 2 && !ratelimit.Waiting(requestCtx, resp.Request.URL) {
				continue
			}
			return nil, fmt.Errorf("mapping HTTP %d; retry later", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("mapping HTTP %d", resp.StatusCode)
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, MaxSnapshotBytes+1))
		closeErr := resp.Body.Close()
		if e != nil || closeErr != nil {
			return nil, errors.New("incomplete mapping snapshot")
		}
		if len(data) > MaxSnapshotBytes {
			return nil, errors.New("mapping snapshot too large")
		}
		return data, nil
	}
	return nil, errors.New("mapping retries exhausted")
}
func (s *Store) UpdateAniBridge(ctx context.Context, rawURL string) (Source, []byte, error) {
	if rawURL == "" {
		rawURL = DefaultAniBridgeURL
	}
	data, e := s.fetch(ctx, rawURL)
	if e != nil {
		return Source{}, nil, e
	}
	source, e := s.LoadAniBridge(data)
	if e != nil {
		return Source{}, nil, e
	}
	return source, data, nil
}
func (s *Store) UpdateAnimeLists(ctx context.Context, rawURL string) (Source, []byte, error) {
	if rawURL == "" {
		rawURL = DefaultAnimeListsURL
	}
	data, e := s.fetch(ctx, rawURL)
	if e != nil {
		return Source{}, nil, e
	}
	source, e := s.LoadAnimeLists(data)
	if e != nil {
		return Source{}, nil, e
	}
	return source, data, nil
}
