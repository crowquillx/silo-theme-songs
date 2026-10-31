// Package mapping resolves catalog seasons to AniDB works using AniBridge v3
// and a separately indexed Anime-Lists fallback. It does not infer destinations.
package mapping

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const MaxSnapshotBytes = 64 << 20

type Source struct{ Version, SHA256 string }
type Span struct {
	First, Last int
	Open        bool
}
type Match struct {
	AniDBID, Scope, Source string
	Coverage               []Span
	CoverageUnknown        bool
}
type Status string

const (
	Resolved  Status = "resolved"
	Missing   Status = "missing"
	Ambiguous Status = "ambiguous"
)

type Result struct {
	Status     Status
	Matches    []Match
	Provenance Source
	Reason     string
}
type Query struct {
	Provider, ID   string
	Season         int
	Episodes       []int
	ManualAniDBIDs []string
}
type edge struct {
	provider, id string
	season       int
	anidb, scope string
	coverage     []Span
	unknown      bool
}
type Snapshot struct {
	Source Source
	edges  []edge
}

func descriptor(raw string) (provider, id, scope string, err error) {
	p := strings.Split(raw, ":")
	if len(p) < 2 || len(p) > 3 || p[0] == "" || p[1] == "" {
		return "", "", "", fmt.Errorf("invalid descriptor %q", raw)
	}
	if len(p) == 3 {
		scope = p[2]
	}
	return p[0], p[1], scope, nil
}
func seasonScope(s string) (int, bool) {
	if !strings.HasPrefix(s, "s") {
		return 0, false
	}
	n, e := strconv.Atoi(s[1:])
	return n, e == nil && n >= 0
}
func aniScope(s string) bool { return slices.Contains([]string{"R", "S", "O", "C", "T", "P"}, s) }
func parseSpan(s string) (Span, error) {
	p := strings.Split(s, "-")
	if len(p) > 2 || len(p) == 0 {
		return Span{}, fmt.Errorf("invalid range %q", s)
	}
	a, e := strconv.Atoi(p[0])
	if e != nil || a < 1 {
		return Span{}, fmt.Errorf("invalid range %q", s)
	}
	if len(p) == 1 {
		return Span{First: a, Last: a}, nil
	}
	if p[1] == "" {
		return Span{First: a, Open: true}, nil
	}
	b, e := strconv.Atoi(p[1])
	if e != nil || b < a {
		return Span{}, fmt.Errorf("invalid range %q", s)
	}
	return Span{First: a, Last: b}, nil
}
func targetCount(s string) (int, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	parts := strings.Split(s, "|")
	if len(parts) > 2 {
		return 0, false, errors.New("invalid target ratio")
	}
	if len(parts) == 2 {
		r, e := strconv.Atoi(parts[1])
		if e != nil {
			return 0, false, e
		}
		if r == 0 {
			return 0, false, nil
		}
	}
	total := 0
	for _, p := range strings.Split(parts[0], ",") {
		span, e := parseSpan(p)
		if e != nil {
			return 0, false, e
		}
		if span.Open {
			return 0, true, nil
		}
		total += span.Last - span.First + 1
	}
	return total, false, nil
}
func mappedSpan(src Span, target string) (Span, bool, error) {
	count, open, e := targetCount(target)
	if e != nil {
		return Span{}, false, e
	}
	if count == 0 && !open {
		return Span{}, false, nil
	}
	// Descriptor-only or non-bijective ranges are useful identity evidence,
	// but they cannot certify a precise episode membership.
	if src.Open || open || (src.Last-src.First+1) != count || strings.Contains(target, "|") {
		return src, true, nil
	}
	return src, false, nil
}
func validID(id string) bool { n, e := strconv.Atoi(id); return e == nil && n > 0 }
func hash(b []byte) string   { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// ParseAniBridge validates the v3 document and indexes only explicit AniDB
// relationships. Null ranges are explicit unmaps and never create a match.
func ParseAniBridge(data []byte) (*Snapshot, error) {
	if len(data) == 0 || len(data) > MaxSnapshotBytes {
		return nil, errors.New("AniBridge snapshot size out of bounds")
	}
	var top map[string]json.RawMessage
	if e := json.Unmarshal(data, &top); e != nil {
		return nil, e
	}
	var meta struct {
		Schema string `json:"schema_version"`
	}
	if e := json.Unmarshal(top["$meta"], &meta); e != nil {
		return nil, fmt.Errorf("AniBridge meta: %w", e)
	}
	if meta.Schema != "3" && !strings.HasPrefix(meta.Schema, "3.") {
		return nil, fmt.Errorf("unsupported AniBridge schema %q", meta.Schema)
	}
	out := &Snapshot{Source: Source{Version: meta.Schema, SHA256: hash(data)}}
	for src, raw := range top {
		if strings.HasPrefix(src, "$") {
			continue
		}
		sp, si, ss, e := descriptor(src)
		if e != nil {
			return nil, e
		}
		var targets map[string]json.RawMessage
		if e = json.Unmarshal(raw, &targets); e != nil {
			return nil, e
		}
		for dst, traw := range targets {
			tp, ti, ts, e := descriptor(dst)
			if e != nil {
				return nil, e
			}
			var provider, id, scope string
			var season int
			var reverse bool
			switch {
			case (sp == "tvdb_show" || sp == "tmdb_show") && tp == "anidb":
				season, _ = seasonScope(ss)
				provider, id, scope = sp, si, ts
				if !validID(ti) || !aniScope(ts) {
					return nil, fmt.Errorf("invalid AniDB target %q", dst)
				}
			case sp == "anidb" && (tp == "tvdb_show" || tp == "tmdb_show"):
				season, reverse = seasonScope(ts)
				provider, id, scope = tp, ti, ss
				reverse = true
				if !validID(si) || !aniScope(ss) {
					return nil, fmt.Errorf("invalid AniDB source %q", src)
				}
			default:
				continue
			}
			if _, ok := seasonScope(func() string {
				if reverse {
					return ts
				}
				return ss
			}()); !ok {
				return nil, fmt.Errorf("invalid season scope %s -> %s", src, dst)
			}
			if !validID(id) {
				return nil, fmt.Errorf("invalid series id %q", id)
			}
			anidb := ti
			if reverse {
				anidb = si
			}
			var ranges map[string]*string
			if e = json.Unmarshal(traw, &ranges); e != nil {
				return nil, e
			}
			for sr, tv := range ranges {
				sourceSpan, e := parseSpan(sr)
				if e != nil {
					return nil, e
				}
				if tv == nil {
					continue
				}
				span, unknown, e := mappedSpan(sourceSpan, *tv)
				if e != nil {
					return nil, e
				}
				if span.First == 0 {
					continue
				}
				if reverse { // source coordinates are AniDB; target coordinates are catalog.
					segments := strings.Split(strings.Split(*tv, "|")[0], ",")
					if len(segments) != 1 {
						unknown = true
					} else if t, e := parseSpan(segments[0]); e == nil {
						span = t
					} else {
						return nil, e
					}
				}
				out.edges = append(out.edges, edge{provider, id, season, anidb, scope, []Span{span}, unknown})
			}
		}
	}
	return out, nil
}

type animeList struct {
	XMLName xml.Name `xml:"anime-list"`
	Entries []struct {
		AniDB      string `xml:"anidbid,attr"`
		TVDB       string `xml:"tvdbid,attr"`
		TVDBSeason string `xml:"defaulttvdbseason,attr"`
		Offset     string `xml:"episodeoffset,attr"`
		TMDB       string `xml:"tmdbtv,attr"`
		TMDBSeason string `xml:"tmdbseason,attr"`
		TMDBOffset string `xml:"tmdboffset,attr"`
		Rows       []struct {
			AniDBSeason string `xml:"anidbseason,attr"`
			TVDBSeason  string `xml:"tvdbseason,attr"`
			TMDBSeason  string `xml:"tmdbseason,attr"`
			Start       string `xml:"start,attr"`
			End         string `xml:"end,attr"`
			Offset      string `xml:"offset,attr"`
			Text        string `xml:",chardata"`
		} `xml:"mapping-list>mapping"`
	} `xml:"anime"`
}

// ParseAnimeLists builds a reverse candidate index. Open offsets remain
// marked uncertain; callers can show them in preview without claiming exact
// episode coverage.
func ParseAnimeLists(data []byte) (*Snapshot, error) {
	if len(data) == 0 || len(data) > MaxSnapshotBytes {
		return nil, errors.New("Anime-Lists snapshot size out of bounds")
	}
	var doc animeList
	if e := xml.Unmarshal(data, &doc); e != nil {
		return nil, e
	}
	if doc.XMLName.Local != "anime-list" {
		return nil, errors.New("expected anime-list root")
	}
	out := &Snapshot{Source: Source{Version: "anime-lists-xml-v1", SHA256: hash(data)}}
	for _, a := range doc.Entries {
		if !validID(a.AniDB) {
			continue
		}
		for _, p := range []struct{ provider, id, season, offset string }{{"tvdb_show", a.TVDB, a.TVDBSeason, a.Offset}, {"tmdb_show", a.TMDB, a.TMDBSeason, a.TMDBOffset}} {
			if !validID(p.id) {
				continue
			}
			addedDefault := false
			defaultSeason, _ := strconv.Atoi(p.season)
			for _, r := range a.Rows {
				seasonRaw := p.season
				if p.provider == "tvdb_show" && r.TVDBSeason != "" {
					seasonRaw = r.TVDBSeason
				}
				if p.provider == "tmdb_show" && r.TMDBSeason != "" {
					seasonRaw = r.TMDBSeason
				}
				season, e := strconv.Atoi(seasonRaw)
				if e != nil || season < 0 {
					continue
				}
				scope := "R"
				if r.AniDBSeason == "0" {
					scope = "S"
				}
				if r.Start != "" {
					start, e1 := strconv.Atoi(r.Start)
					end, e2 := strconv.Atoi(r.End)
					off, e3 := strconv.Atoi(r.Offset)
					if r.Offset == "" {
						off, e3 = 0, nil
					}
					if r.End == "" {
						end, e2 = start, nil
					}
					if e1 == nil && e2 == nil && e3 == nil && start > 0 && end >= start && start+off > 0 {
						out.edges = append(out.edges, edge{p.provider, p.id, season, a.AniDB, scope, []Span{{First: start + off, Last: end + off}}, false})
						if season == defaultSeason && scope == "R" {
							addedDefault = true
						}
					}
				}
				for _, token := range strings.Split(r.Text, ";") {
					parts := strings.SplitN(strings.TrimSpace(token), "-", 2)
					if len(parts) != 2 {
						continue
					}
					ani, e1 := strconv.Atoi(parts[0])
					target, e2 := strconv.Atoi(parts[1])
					if e1 == nil && e2 == nil && ani > 0 && target > 0 {
						out.edges = append(out.edges, edge{p.provider, p.id, season, a.AniDB, scope, []Span{{First: target, Last: target}}, false})
						if season == defaultSeason && scope == "R" {
							addedDefault = true
						}
					}
				}
			}
			if !addedDefault {
				season, e1 := strconv.Atoi(p.season)
				off, e2 := strconv.Atoi(p.offset)
				if p.offset == "" {
					off, e2 = 0, nil
				}
				if e1 == nil && e2 == nil && season > 0 {
					first := 1 + off
					if first < 1 {
						first = 1
					}
					out.edges = append(out.edges, edge{p.provider, p.id, season, a.AniDB, "R", []Span{{First: first, Open: true}}, true})
				}
			}
		}
	}
	// Adjacent title-level offsets describe successive cours. Bound each
	// open candidate at the next distinct catalog start. These inferred
	// bounds remain marked uncertain rather than claiming exact membership.
	for i := range out.edges {
		e := &out.edges[i]
		if !e.unknown || len(e.coverage) != 1 || !e.coverage[0].Open {
			continue
		}
		start, next := e.coverage[0].First, 0
		for _, other := range out.edges {
			if other.provider != e.provider || other.id != e.id || other.season != e.season || other.anidb == e.anidb || len(other.coverage) == 0 {
				continue
			}
			candidate := other.coverage[0].First
			if candidate > start && (next == 0 || candidate < next) {
				next = candidate
			}
		}
		if next > start {
			e.coverage[0].Last = next - 1
			e.coverage[0].Open = false
		}
	}
	return out, nil
}

type Resolver struct{ primary, fallback *Snapshot }

func NewResolver(primary, fallback *Snapshot) *Resolver { return &Resolver{primary, fallback} }
func covers(s Span, ep int) bool                        { return ep >= s.First && (s.Open || ep <= s.Last) }
func relevant(e edge, q Query) bool {
	if e.provider != q.Provider || e.id != q.ID || e.season != q.Season {
		return false
	}
	if len(q.Episodes) == 0 {
		return true
	}
	for _, ep := range q.Episodes {
		for _, s := range e.coverage {
			if covers(s, ep) {
				return true
			}
		}
	}
	return false
}
func resolveSnapshot(s *Snapshot, q Query, source string) Result {
	if s == nil {
		return Result{Status: Missing}
	}
	m := map[string]*Match{}
	for _, e := range s.edges {
		if !relevant(e, q) {
			continue
		}
		key := e.anidb + ":" + e.scope
		hit := m[key]
		if hit == nil {
			hit = &Match{AniDBID: e.anidb, Scope: e.scope, Source: source}
			m[key] = hit
		}
		hit.Coverage = append(hit.Coverage, e.coverage...)
		hit.CoverageUnknown = hit.CoverageUnknown || e.unknown
	}
	out := Result{Status: Missing, Provenance: s.Source}
	for _, v := range m {
		out.Matches = append(out.Matches, *v)
	}
	slices.SortFunc(out.Matches, func(a, b Match) int {
		ai, _ := strconv.Atoi(a.AniDBID)
		bi, _ := strconv.Atoi(b.AniDBID)
		if ai != bi {
			return ai - bi
		}
		return strings.Compare(a.Scope, b.Scope)
	})
	if len(out.Matches) == 0 {
		return out
	}
	out.Status = Resolved
	// Multiple disjoint cours are valid. Overlap across different AniDB IDs
	// leaves the season unresolved; duplicate routes to the same ID were merged.
	for i, a := range out.Matches {
		for _, b := range out.Matches[i+1:] {
			for _, x := range a.Coverage {
				for _, y := range b.Coverage {
					if (x.Open || y.First <= x.Last) && (y.Open || x.First <= y.Last) {
						out.Status = Ambiguous
						out.Reason = "different AniDB entries cover the same catalog episodes"
						return out
					}
				}
			}
		}
	}
	return out
}

// Resolve uses manual IDs first, then AniBridge, then Anime-Lists only if
// AniBridge has no candidate. A primary ambiguity never activates fallback.
func (r *Resolver) Resolve(q Query) Result {
	if q.Provider != "tvdb_show" && q.Provider != "tmdb_show" || !validID(q.ID) || q.Season < 0 {
		return Result{Status: Missing, Reason: "invalid catalog coordinate"}
	}
	if len(q.ManualAniDBIDs) > 0 {
		out := Result{Status: Resolved, Provenance: Source{Version: "manual"}}
		seen := map[string]bool{}
		for _, id := range q.ManualAniDBIDs {
			if !validID(id) {
				return Result{Status: Missing, Reason: "invalid manual AniDB ID"}
			}
			if !seen[id] {
				out.Matches = append(out.Matches, Match{AniDBID: id, Scope: "R", Source: "manual", CoverageUnknown: true})
				seen[id] = true
			}
		}
		return out
	}
	out := resolveSnapshot(r.primary, q, "anibridge")
	if out.Status != Missing {
		return out
	}
	return resolveSnapshot(r.fallback, q, "anime-lists")
}
