// Package provider selects theme sources and stages audio for a separate,
// ownership-aware publisher. It has no Silo or filesystem destination policy.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

type Code string

const (
	MissingID        Code = "missing_id"
	Absent           Code = "absent"
	Malformed        Code = "malformed"
	Transient        Code = "transient"
	UnsafeURL        Code = "unsafe_url"
	UnavailableMedia Code = "unavailable_media"
	MissingTool      Code = "missing_tool"
	ExtractorBroken  Code = "extractor_broken"
	LimitExceeded    Code = "limit_exceeded"
	InvalidAudio     Code = "invalid_audio"
)

// Error contains no URL, credential, subprocess output, or response body.
type Error struct {
	Code Code
	Op   string
}

func (e *Error) Error() string { return fmt.Sprintf("provider %s: %s", e.Op, e.Code) }
func IsCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

type Kind string

const (
	TV    Kind = "tv"
	Movie Kind = "movie"
)

type IDs struct {
	TMDB string
	TVDB string
	IMDB string
}

// Request uses an override first, an explicitly selected template second,
// then an exact ThemerrDB TMDB lookup. Neither lookup errors nor absence
// trigger a title search or a different source.
type Request struct {
	Kind        Kind
	IDs         IDs
	OverrideURL string
	Template    string
}

type Source struct {
	URL        string
	Origin     string // override, template, or themerrdb
	ProviderID string // TMDB ID for ThemerrDB
	Format     string // mp3 etc. for direct audio, or mp3 for extraction
	Extract    bool
}

type Resolver struct {
	DB *ThemerrDB
	// AllowedDirectHosts names the HTTPS hosts from which direct audio may be
	// downloaded. YouTube video hosts are separately fixed below.
	AllowedDirectHosts []string
}

var numericID = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
var imdbID = regexp.MustCompile(`^tt[0-9]{7,10}$`)
var placeholder = regexp.MustCompile(`\{(tmdbId|tvdbId|imdbId)\}`)
var anyBrace = regexp.MustCompile(`[{}]`)

func (r *Resolver) Resolve(ctx context.Context, req Request) (Source, error) {
	if req.Kind != TV && req.Kind != Movie {
		return Source{}, &Error{Malformed, "kind"}
	}
	if req.OverrideURL != "" {
		return r.source(req.OverrideURL, "override", "")
	}
	if req.Template != "" {
		value, err := expandTemplate(req.Template, req.IDs)
		if err != nil {
			return Source{}, err
		}
		return r.source(value, "template", "")
	}
	if !numericID.MatchString(req.IDs.TMDB) {
		return Source{}, &Error{MissingID, "tmdb"}
	}
	if r.DB == nil {
		return Source{}, &Error{Malformed, "themerrdb configuration"}
	}
	value, err := r.DB.Lookup(ctx, req.Kind, req.IDs.TMDB)
	if err != nil {
		return Source{}, err
	}
	s, err := r.source(value, "themerrdb", req.IDs.TMDB)
	if err != nil {
		return Source{}, &Error{Malformed, "themerrdb URL"}
	}
	if !s.Extract {
		return Source{}, &Error{Malformed, "themerrdb URL"}
	}
	return s, nil
}

func expandTemplate(t string, ids IDs) (string, error) {
	u, err := url.Parse(t)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || anyBrace.MatchString(u.Host) {
		return "", &Error{UnsafeURL, "template"}
	}
	values := map[string]string{"tmdbId": ids.TMDB, "tvdbId": ids.TVDB, "imdbId": ids.IMDB}
	for _, m := range placeholder.FindAllStringSubmatch(t, -1) {
		v := values[m[1]]
		if (m[1] == "imdbId" && !imdbID.MatchString(v)) || (m[1] != "imdbId" && !numericID.MatchString(v)) {
			return "", &Error{MissingID, m[1]}
		}
		t = strings.ReplaceAll(t, m[0], v)
	}
	if anyBrace.MatchString(t) {
		return "", &Error{Malformed, "template placeholder"}
	}
	return t, nil
}

var directFormats = map[string]bool{".mp3": true, ".m4a": true, ".m4b": true, ".flac": true, ".ogg": true, ".opus": true, ".wav": true, ".aac": true}

func (r *Resolver) source(raw, origin, id string) (Source, error) {
	u, err := safeURL(raw)
	if err != nil {
		return Source{}, err
	}
	host := strings.ToLower(u.Hostname())
	if isYouTube(host) {
		if !validVideoURL(u) {
			return Source{}, &Error{UnsafeURL, "video URL"}
		}
		return Source{URL: u.String(), Origin: origin, ProviderID: id, Format: "mp3", Extract: true}, nil
	}
	if !hostAllowed(host, r.AllowedDirectHosts) {
		return Source{}, &Error{UnsafeURL, "direct host"}
	}
	ext := strings.ToLower(pathExt(u.EscapedPath()))
	if !directFormats[ext] {
		return Source{}, &Error{UnsafeURL, "direct format"}
	}
	return Source{URL: u.String(), Origin: origin, ProviderID: id, Format: strings.TrimPrefix(ext, ".")}, nil
}

func safeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Port() != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, &Error{UnsafeURL, "URL"}
	}
	h := u.Hostname()
	if net.ParseIP(h) != nil || strings.HasSuffix(strings.ToLower(h), ".localhost") || strings.EqualFold(h, "localhost") || !strings.Contains(h, ".") {
		return nil, &Error{UnsafeURL, "host"}
	}
	return u, nil
}

func hostAllowed(host string, allow []string) bool {
	for _, v := range allow {
		if strings.EqualFold(host, v) && net.ParseIP(v) == nil && strings.Contains(v, ".") {
			return true
		}
	}
	return false
}

func isYouTube(host string) bool {
	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "www.youtube-nocookie.com":
		return true
	}
	return false
}

func validVideoURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "youtu.be" {
		return regexp.MustCompile(`^/[A-Za-z0-9_-]{11}$`).MatchString(u.Path) && u.RawQuery == ""
	}
	if u.Path != "/watch" || len(u.Query()) != 1 || len(u.Query()["v"]) != 1 {
		return false
	}
	return regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`).MatchString(u.Query().Get("v"))
}

func pathExt(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i < 0 {
		return ""
	}
	return p[i:]
}
