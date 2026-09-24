package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDBURL = "https://app.lizardbyte.dev/ThemerrDB"

type dbEntry struct {
	url     string
	code    Code
	expires time.Time
	used    time.Time
}

// ThemerrDB is a bounded, in-memory exact TMDB lookup client. A new process
// starts with an empty cache; callers persist chosen URLs in their own state.
type ThemerrDB struct {
	BaseURL     string // empty uses the official endpoint; HTTP only for loopback tests
	Client      *http.Client
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	MaxEntries  int

	mu    sync.Mutex
	cache map[string]dbEntry
}

func (d *ThemerrDB) Lookup(ctx context.Context, kind Kind, tmdbID string) (string, error) {
	if (kind != TV && kind != Movie) || !numericID.MatchString(tmdbID) {
		return "", &Error{MissingID, "themerrdb"}
	}
	key := string(kind) + "/" + tmdbID
	now := time.Now()
	d.mu.Lock()
	if entry, ok := d.cache[key]; ok && now.Before(entry.expires) {
		entry.used = now
		d.cache[key] = entry
		d.mu.Unlock()
		if entry.code != "" {
			return "", &Error{entry.code, "themerrdb"}
		}
		return entry.url, nil
	}
	d.mu.Unlock()
	base := d.BaseURL
	if base == "" {
		base = defaultDBURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", &Error{Malformed, "themerrdb endpoint"}
	}
	path := "tv_shows"
	if kind == Movie {
		path = "movies"
	}
	endpoint := strings.TrimRight(base, "/") + "/" + path + "/themoviedb/" + tmdbID + ".json"
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	copyClient := *client
	if copyClient.Timeout == 0 || copyClient.Timeout > 15*time.Second {
		copyClient.Timeout = 15 * time.Second
	}
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	var value string
	var code Code
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		req.Header.Set("Accept", "application/json")
		resp, e := copyClient.Do(req)
		if ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return "", ctx.Err()
		}
		if e != nil {
			code = Transient
		} else {
			value, code = parseDBResponse(resp)
			resp.Body.Close()
		}
		if code != Transient || attempt == 2 {
			break
		}
		wait := time.Duration(100*(1<<attempt)) * time.Millisecond
		if resp != nil {
			wait = retryDelay(resp.Header.Get("Retry-After"), wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if code == "" || code == Absent {
		ttl := d.PositiveTTL
		if code == Absent {
			ttl = d.NegativeTTL
		}
		if ttl <= 0 {
			if code == Absent {
				ttl = 30 * time.Minute
			} else {
				ttl = 6 * time.Hour
			}
		}
		if ttl > 24*time.Hour {
			ttl = 24 * time.Hour
		}
		max := d.MaxEntries
		if max <= 0 {
			max = 256
		}
		if max > 4096 {
			max = 4096
		}
		d.mu.Lock()
		if d.cache == nil {
			d.cache = make(map[string]dbEntry)
		}
		if len(d.cache) >= max {
			oldest := ""
			var when time.Time
			for k, entry := range d.cache {
				if oldest == "" || entry.used.Before(when) {
					oldest, when = k, entry.used
				}
			}
			delete(d.cache, oldest)
		}
		d.cache[key] = dbEntry{value, code, time.Now().Add(ttl), time.Now()}
		d.mu.Unlock()
	}
	if code != "" {
		return "", &Error{code, "themerrdb"}
	}
	return value, nil
}

func parseDBResponse(resp *http.Response) (string, Code) {
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return "", Absent
	case http.StatusOK:
	default:
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return "", Transient
		}
		return "", Malformed
	}
	if resp.ContentLength > 128*1024 {
		return "", Malformed
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	if err != nil || len(data) > 128*1024 {
		return "", Malformed
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return "", Malformed
	}
	raw, ok := obj["youtube_theme_url"]
	if !ok || string(raw) == "null" {
		return "", Absent
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", Malformed
	}
	if value == "" {
		return "", Absent
	}
	return value, ""
}

func retryDelay(header string, fallback time.Duration) time.Duration {
	if n, err := strconv.Atoi(header); err == nil && n >= 0 {
		if n > 2 {
			n = 2
		}
		return time.Duration(n) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		d := time.Until(when)
		if d < 0 {
			return 0
		}
		if d > 2*time.Second {
			return 2 * time.Second
		}
		return d
	}
	return fallback
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }
