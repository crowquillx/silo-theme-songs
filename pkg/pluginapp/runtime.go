package pluginapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/engine"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type Row struct {
	Item        string                     `json:"item"`
	ItemID      string                     `json:"item_id"`
	Season      int                        `json:"season"`
	Destination string                     `json:"destination,omitempty"`
	Source      string                     `json:"source,omitempty"`
	Theme       string                     `json:"theme,omitempty"`
	Action      string                     `json:"action"`
	Reason      string                     `json:"reason,omitempty"`
	Candidates  []provider.ReviewCandidate `json:"candidates,omitempty"`
}
type Report struct {
	State          string    `json:"state"`
	Mode           string    `json:"mode"`
	At             time.Time `json:"at"`
	Rows           []Row     `json:"rows"`
	Errors         []string  `json:"errors,omitempty"`
	Downloaded     int       `json:"downloaded"`
	CatalogAdapter string    `json:"catalog_adapter"`
	Truncated      bool      `json:"truncated,omitempty"`
	RepairCount    int       `json:"repair_count,omitempty"`
	RepairPaths    []string  `json:"repair_paths,omitempty"`
}

func (r *Report) add(row Row) {
	if len(r.Rows) >= 1000 {
		r.Truncated = true
		return
	}
	r.Rows = append(r.Rows, row)
}

type Server struct {
	runtimedefault.Server
	pluginv1.UnimplementedScheduledTaskServer
	pluginv1.UnimplementedScanSourceServer
	pluginv1.UnimplementedHttpRoutesServer
	mu          sync.RWMutex
	work        sync.Mutex
	manifest    *pluginv1.PluginManifest
	name        string
	anime       bool
	factory     Factory
	cfg         Config
	client      *siloapi.Client
	provider    Provider
	engine      *engine.Engine
	report      Report
	repairCount int
	repairPaths []string
}

func New(m *pluginv1.PluginManifest, name string, anime bool, f Factory) *Server {
	return &Server{manifest: m, name: name, anime: anime, factory: f, report: Report{State: "not_configured", Rows: []Row{}}}
}
func (s *Server) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}
func (s *Server) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	var raw []byte
	for _, entry := range req.GetConfig() {
		if entry.GetKey() == "settings" {
			var err error
			raw, err = json.Marshal(entry.GetValue().AsMap())
			if err != nil {
				return nil, errors.New("invalid plugin settings")
			}
		}
	}
	if len(raw) == 0 || string(raw) == "{}" {
		return &pluginv1.ConfigureResponse{}, s.resetLocal()
	}
	cfg := Defaults()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&cfg); e != nil {
		return nil, errors.New("invalid plugin settings")
	}
	if e := s.ConfigureLocal(ctx, cfg); e != nil {
		return nil, e
	}
	return &pluginv1.ConfigureResponse{}, nil
}
func (s *Server) resetLocal() error {
	s.work.Lock()
	defer s.work.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.engine != nil {
		err = s.engine.Close()
	}
	s.engine = nil
	s.client = nil
	s.provider = nil
	s.cfg = Config{}
	s.report = Report{State: "not_configured", Rows: []Row{}}
	s.repairCount = 0
	s.repairPaths = nil
	return err
}
func (s *Server) ConfigureLocal(_ context.Context, cfg Config) error {
	if e := cfg.Validate(); e != nil {
		return e
	}
	client, e := siloapi.New(cfg.BaseURL, cfg.APIKey, cfg.ProfileID)
	if e != nil {
		return e
	}
	s.work.Lock()
	defer s.work.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != nil && s.cfg.StateDir != cfg.StateDir {
		return errors.New("restart the plugin before changing state_dir")
	}
	store := s.engine
	if store == nil {
		store, e = engine.Open(cfg.StateDir, s.name)
		if e != nil {
			return e
		}
	}
	provider, e := s.factory(cfg)
	if e != nil {
		if s.engine == nil {
			store.Close()
		}
		return e
	}
	s.cfg = cfg
	s.client = client
	s.provider = provider
	s.engine = store
	s.report = Report{State: "configured", Rows: []Row{}, RepairCount: s.repairCount, RepairPaths: append([]string(nil), s.repairPaths...)}
	if s.repairCount > 0 {
		s.report.State = "needs_attention"
	}
	return nil
}
func (s *Server) Close() error {
	s.work.Lock()
	defer s.work.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != nil {
		return s.engine.Close()
	}
	return nil
}
func taskMode(key string) string {
	for _, v := range []string{"preview", "reconcile", "sync"} {
		if key == v || strings.HasSuffix(key, ":"+v) || strings.HasSuffix(key, "_"+v) || strings.HasSuffix(key, "/"+v) {
			return v
		}
	}
	return ""
}
func (s *Server) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	mode := taskMode(req.GetTaskKey())
	if mode == "" {
		return nil, errors.New("unknown theme task")
	}
	report := s.Execute(ctx, mode)
	data, e := json.Marshal(report)
	if e != nil {
		return nil, e
	}
	var m map[string]any
	if e := json.Unmarshal(data, &m); e != nil {
		return nil, e
	}
	out, e := structpb.NewStruct(m)
	return &pluginv1.RunScheduledTaskResponse{Output: out}, e
}
func generation(c Config, source siloapi.ScanSource) string {
	h := sha256.Sum256([]byte(c.BaseURL + "\x00" + source.ID + "\x00" + c.SourceGeneration))
	return hex.EncodeToString(h[:])
}

func (s *Server) Execute(ctx context.Context, mode string) (report Report) {
	s.work.Lock()
	defer s.work.Unlock()
	report = Report{State: "complete", Mode: mode, At: time.Now().UTC(), Rows: []Row{}, CatalogAdapter: "native_v2"}
	defer func() {
		s.mu.Lock()
		report.RepairCount = s.repairCount
		report.RepairPaths = append([]string(nil), s.repairPaths...)
		if report.RepairCount > 0 && report.State != "prerequisite_failed" && report.State != "cancelled" {
			report.State = "needs_attention"
		}
		s.report = report
		s.mu.Unlock()
	}()
	s.mu.RLock()
	c, api, p, store := s.cfg, s.client, s.provider, s.engine
	s.mu.RUnlock()
	if store == nil {
		report.State = "not_configured"
		report.Errors = []string{"Configure the Silo connection, libraries, writable roots and persistent state."}
		return
	}
	preview := mode == "preview" || mode == "reconcile" || c.PreviewOnly
	if e := api.ThemeCapability(ctx); e != nil {
		report.State = "prerequisite_failed"
		report.Errors = append(report.Errors, e.Error())
		preview = true
	}
	libs, e := api.Libraries(ctx)
	if e != nil {
		report.State = "prerequisite_failed"
		report.Errors = append(report.Errors, e.Error())
		return
	}
	if host := sdkruntime.Host(); host != nil {
		hostLibs, e := host.ListLibraries(ctx, "")
		if e != nil {
			report.State = "prerequisite_failed"
			report.Errors = append(report.Errors, "RuntimeHost.ListLibraries failed")
			return
		}
		for _, id := range c.LibraryIDs {
			if !slices.ContainsFunc(hostLibs, func(l *pluginv1.Library) bool { return l.GetId() == id }) {
				report.State = "prerequisite_failed"
				report.Errors = append(report.Errors, "selected library is not visible through RuntimeHost")
				return
			}
		}
		_, e = host.ListLibraryMedia(ctx, runtimehost.ListLibraryMediaRequest{LibraryIDs: c.LibraryIDs, MediaTypes: []string{"series", "movie"}, PageSize: 1})
		if e != nil && status.Code(e) != codes.Unimplemented {
			report.State = "prerequisite_failed"
			report.Errors = append(report.Errors, "RuntimeHost.ListLibraryMedia failed")
			return
		}
		// V2 always supplies paths and authoritative season memberships; the
		// explicit adapter also handles hosts without catalog enumeration RPC.
	}
	scanGeneration := ""
	scanReady := c.ManualRefresh
	if !c.ManualRefresh {
		source, err := api.ScanSource(ctx, s.manifest.GetPluginId())
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			preview = true
		} else {
			scanGeneration = generation(c, source)
			scanReady = store.Ready(scanGeneration)
			if !scanReady {
				report.Errors = append(report.Errors, "waiting for Autoscan to acknowledge its initial marker")
				preview = true
			}
		}
	}
	if len(report.Errors) > 0 {
		report.State = "prerequisite_failed"
	}
	if mode != "preview" {
		if scanReady {
			if _, e := store.Recover(ctx); e != nil {
				report.Errors = append(report.Errors, e.Error())
				report.State = "needs_attention"
			}
		} else {
			for _, r := range store.Records() {
				if r.Status == engine.Pending {
					report.add(Row{ItemID: r.Destination.ItemID, Season: r.Destination.Season, Destination: r.ServerPath, Action: "pending", Reason: "crash recovery waits for scan source acknowledgement"})
				}
			}
		}
	}
	reconcileRequired := false
	reconcileOK := true
	reconciledOwners := map[string]bool{}
	if !c.ManualRefresh && scanReady && report.State != "prerequisite_failed" {
		last, err := readReconciledGeneration(c.StateDir)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			report.State = "prerequisite_failed"
			preview = true
		} else if last != scanGeneration {
			managed := false
			for _, r := range store.Records() {
				if r.Status != engine.Failed {
					managed = true
					break
				}
			}
			if managed {
				reconcileRequired = true
				if mode != "reconcile" {
					preview = true
					report.State = "reconcile_required"
					for _, r := range store.Records() {
						if r.Status != engine.Failed {
							report.add(Row{ItemID: r.Destination.ItemID, Season: r.Destination.Season, Destination: r.ServerPath, Action: "reconcile_required", Reason: "run the Reconcile task for the acknowledged scan source"})
						}
					}
				}
			} else if mode != "preview" && !preview {
				if err := writeReconciledGeneration(c.StateDir, scanGeneration); err != nil {
					report.Errors = append(report.Errors, err.Error())
					report.State = "prerequisite_failed"
					preview = true
				}
			}
		}
	}
	found := map[string]bool{}
	for _, lib := range libs {
		if !slices.Contains(c.LibraryIDs, lib.ID) {
			continue
		}
		found[lib.ID] = true
		if !lib.Enabled {
			report.add(Row{Item: lib.Name, Action: "refused", Reason: "library is disabled"})
			continue
		}
		if !s.anime && slices.Contains(c.AnimeLibraryIDs, lib.ID) {
			report.add(Row{Item: lib.Name, Action: "excluded", Reason: "anime-managed library"})
			continue
		}
		inv, err := api.Inventory(ctx, lib)
		if err != nil {
			report.add(Row{Item: lib.Name, Action: "refused", Reason: err.Error()})
			continue
		}
		// Reconcile every previously managed fallback on every normal run,
		// including items that no longer have selected sources.
		checked := map[string]bool{}
		for _, r := range store.Records() {
			if r.LibraryID == lib.ID && !checked[r.LocalPath] && mode != "preview" {
				checked[r.LocalPath] = true
				reconciledOwners[r.LibraryID+"\x00"+r.LocalPath] = true
				if _, e := store.Check(ctx, r.Destination); e != nil {
					report.Errors = append(report.Errors, e.Error())
					reconcileOK = false
				}
				if mode == "reconcile" && report.State != "prerequisite_failed" {
					if e := revalidate(ctx, api, c, r.Destination); e != nil {
						reconcileOK = false
						recordRevalidationFailure(&report, store, r, e)
					} else if _, e = store.Reconcile(ctx, &r.Destination); e != nil {
						reconcileOK = false
						report.Errors = append(report.Errors, e.Error())
					}
				}
			}
			if r.LibraryID == lib.ID && r.Destination.Fallback {
				if e := revalidate(ctx, api, c, r.Destination); e != nil {
					reconcileOK = false
					recordRevalidationFailure(&report, store, r, e)
				}
			}
		}
		if mode != "preview" && !(reconcileRequired && mode != "reconcile") {
			s.checkDiscovery(ctx, api, store, lib.ID, &report, mode == "reconcile")
		}
		for _, item := range inv.Items {
			if e := ctx.Err(); e != nil {
				report.State = "cancelled"
				return
			}
			if slices.Contains(c.ExcludeItems, item.ID) {
				continue
			}
			if s.anime && item.Type != "series" {
				continue
			}
			targets := []Target{{Item: item}}
			if s.anime {
				for _, season := range inv.Seasons[item.ID] {
					eps := []int{}
					for _, f := range inv.Files {
						if f.ItemID == item.ID && f.Season == season.Number {
							eps = append(eps, f.Episode)
						}
					}
					if len(eps) > 0 {
						targets = append(targets, Target{Item: item, Season: &season, Episodes: eps})
					}
				}
			}
			for _, target := range targets {
				sn := -1
				if target.Season != nil {
					sn = target.Season.Number
				}
				row := Row{Item: item.Title, ItemID: item.ID, Season: sn}
				dests, err := c.Policy.Resolve(inv, item, target.Season)
				if err != nil {
					row.Action = "refused"
					row.Reason = err.Error()
					report.add(row)
					continue
				}
				candidates, err := p.Select(ctx, target)
				if err != nil {
					var review *provider.ReviewRequired
					if errors.As(err, &review) {
						row.Action = "needs_review"
						row.Reason = "Review the candidate links, then save one URL under provider.url_overrides for this item before extraction."
						row.Candidates = reviewCandidates(review.Candidates)
						report.add(row)
						continue
					}
					row.Action = "source_unavailable"
					row.Reason = err.Error()
					report.add(row)
					continue
				}
				if s.anime {
					reportObsolete(&report, store.Records(), dests, candidates)
				}
				if len(candidates) == 0 {
					row.Action = "no_theme"
					report.add(row)
					continue
				}
				for _, dest := range dests {
					for _, candidate := range candidates {
						r := row
						r.Destination = dest.ServerPath
						r.Source = candidate.Provenance
						r.Theme = candidate.Title
						if dest.Fallback {
							r.Reason = "series-owned theme, inherited by season/episodes; the series page plays it too"
						}
						if candidate.ID == "" || !audioName("theme."+strings.TrimPrefix(candidate.Extension, ".")) {
							r.Action = "source_unavailable"
							r.Reason = "candidate has no stable ID or supported audio format"
							report.add(r)
							continue
						}
						stableID := sourceID(candidate)
						if !s.anime && changedSelection(store.Records(), dest, stableID) {
							r.Action = "selection_changed"
							r.Reason = "existing managed theme preserved; review the changed source selection"
							report.add(r)
							continue
						}
						if reason := existingAudio(dest, store.Records()); reason != "" {
							r.Action = "preserved"
							r.Reason = reason
							report.add(r)
							continue
						}
						if old, ok := existing(store.Records(), dest, stableID); ok {
							r.Action = string(old.Status)
							if old.Status == engine.Published {
								r.Action = "destination_unconfirmed"
							}
							report.add(r)
							continue
						}
						if preflight, ok := p.(interface {
							Preflight(context.Context, Candidate) error
						}); ok {
							if e := preflight.Preflight(ctx, candidate); e != nil {
								r.Action = "prerequisite_failed"
								r.Reason = e.Error()
								report.add(r)
								continue
							}
						}
						if preview {
							r.Action = "would_download"
							report.add(r)
							continue
						}
						if report.Downloaded >= c.MaxDownloads {
							r.Action = "deferred"
							r.Reason = "per-run download limit"
							report.add(r)
							continue
						}
						if blocked(store.Records(), dest) {
							r.Action = "destination_unconfirmed"
							r.Reason = "existing theme needs discovery or operator review"
							report.add(r)
							continue
						}
						staged, err := p.Fetch(ctx, candidate)
						if err != nil {
							r.Action = "download_failed"
							r.Reason = err.Error()
							report.add(r)
							continue
						}
						if staged.Format != "mp3" {
							err = errors.New("provider did not produce MP3 audio")
						} else {
							output := candidate
							output.Extension = staged.Format
							_, err = store.Publish(ctx, dest, stableID, filename(s.name, output), staged.Path, func(ctx context.Context) error { return revalidate(ctx, api, c, dest) })
						}
						if closeErr := staged.Close(); closeErr != nil {
							report.Errors = append(report.Errors, closeErr.Error())
						}
						if err != nil {
							r.Action = "refused"
							r.Reason = err.Error()
						} else {
							r.Action = "downloaded"
							report.Downloaded++
							if c.ManualRefresh {
								r.Reason = "run a library scan, then the reconcile task"
							} else {
								r.Reason = "awaiting Autoscan and exact owner discovery"
							}
						}
						report.add(r)
					}
				}
			}
		}
	}
	for _, id := range c.LibraryIDs {
		if !found[id] {
			report.Errors = append(report.Errors, "selected library is unavailable: "+id)
			report.State = "prerequisite_failed"
		}
	}
	if mode == "reconcile" && !c.ManualRefresh && scanReady && report.State != "prerequisite_failed" {
		for _, r := range store.Records() {
			if r.Status == engine.Failed {
				continue
			}
			if !reconciledOwners[r.LibraryID+"\x00"+r.LocalPath] || r.Status == engine.Pending || r.Status == engine.Protected || r.Status == engine.Stale {
				reconcileOK = false
			}
		}
		if reconcileOK {
			if err := writeReconciledGeneration(c.StateDir, scanGeneration); err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
		} else if reconcileRequired {
			report.State = "reconcile_required"
		}
	}
	if len(report.Errors) > 0 && report.State != "prerequisite_failed" && report.State != "cancelled" {
		report.State = "needs_attention"
	}
	if report.State == "complete" {
		hasReview := false
		if len(report.Errors) > 0 {
			report.State = "needs_attention"
		}
		for _, r := range report.Rows {
			if r.Action == "needs_review" {
				hasReview = true
			}
			if r.Action == "prerequisite_failed" {
				report.State = "prerequisite_failed"
				break
			}
			if r.Action == "refused" || r.Action == "download_failed" || r.Action == "source_unavailable" || r.Action == "stale" || r.Action == "protected" || r.Action == "destination_unconfirmed" || r.Action == "selection_changed" || r.Action == "operator_review" {
				report.State = "needs_attention"
			}
		}
		if hasReview && report.State == "complete" {
			report.State = "needs_review"
		}
	}
	return
}
func existing(records []engine.Record, d destinations.Destination, id string) (engine.Record, bool) {
	for _, r := range records {
		if r.LocalPath == d.LocalPath && r.SourceID == id && r.Status != engine.Failed {
			return r, true
		}
	}
	return engine.Record{}, false
}
func blocked(records []engine.Record, d destinations.Destination) bool {
	for _, r := range records {
		if r.LocalPath == d.LocalPath && (r.Status == engine.Published || r.Status == engine.Pending || r.Status == engine.Stale || r.Status == engine.Protected) {
			return true
		}
	}
	return false
}

func changedSelection(records []engine.Record, d destinations.Destination, id string) bool {
	for _, r := range records {
		if r.LibraryID == d.LibraryID && r.LocalPath == d.LocalPath && r.Status != engine.Failed && r.SourceID != id {
			return true
		}
	}
	return false
}

func reportObsolete(report *Report, records []engine.Record, dests []destinations.Destination, candidates []Candidate) {
	selected := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c.ID != "" {
			selected[sourceID(c)] = true
		}
	}
	for _, d := range dests {
		for _, r := range records {
			if r.LibraryID != d.LibraryID || r.LocalPath != d.LocalPath || r.Status == engine.Failed || selected[r.SourceID] {
				continue
			}
			if slices.ContainsFunc(report.Rows, func(row Row) bool {
				return row.Action == "operator_review" && row.Destination == r.ServerPath && row.Theme == r.Filename
			}) {
				continue
			}
			report.add(Row{ItemID: d.ItemID, Season: d.Season, Destination: r.ServerPath, Source: r.SourceID, Theme: r.Filename, Action: "operator_review", Reason: "managed theme is absent from the current selection; file preserved"})
		}
	}
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)
var reviewVideoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var safeSourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,199}$`)
var visibleSourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,63}$`)

func sourceID(c Candidate) string {
	if safeSourceID.MatchString(c.ID) {
		return c.ID
	}
	h := sha256.Sum256([]byte(c.ID))
	return "hash:" + hex.EncodeToString(h[:])
}

func filenameID(id string) string {
	if visibleSourceID.MatchString(id) {
		encoded := strings.NewReplacer("_", "_u", "-", "_h", ":", "-", ".", "_d").Replace(id)
		if len(encoded) <= 80 {
			return "id-" + encoded
		}
	}
	h := sha256.Sum256([]byte(id))
	return "hash-" + hex.EncodeToString(h[:8])
}

func reviewCandidates(in []provider.ReviewCandidate) []provider.ReviewCandidate {
	var out []provider.ReviewCandidate
	for _, c := range in {
		if len(out) == 8 {
			break
		}
		if !reviewVideoID.MatchString(c.ID) {
			continue
		}
		c.URL = "https://www.youtube.com/watch?v=" + c.ID
		c.Title = limitRunes(c.Title, 200)
		c.Channel = limitRunes(c.Channel, 100)
		if math.IsNaN(c.Duration) || math.IsInf(c.Duration, 0) || c.Duration < 0 || c.Duration > 900 {
			c.Duration = 0
		}
		if len(c.Reasons) > 5 {
			c.Reasons = c.Reasons[:5]
		}
		for i := range c.Reasons {
			c.Reasons[i] = limitRunes(c.Reasons[i], 200)
		}
		out = append(out, c)
	}
	return out
}

func limitRunes(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

func filename(name string, c Candidate) string {
	slug := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(c.Title), "-"), "-")
	if len(slug) > 45 {
		slug = slug[:45]
	}
	if slug == "" {
		slug = "theme"
	}
	return fmt.Sprintf("%s-%s-%s.%s", name, filenameID(c.ID), slug, strings.ToLower(strings.TrimPrefix(c.Extension, ".")))
}

// A failed API request cannot establish that an existing destination changed.
var errDestinationChanged = errors.New("destination ownership changed since preview")

func recordRevalidationFailure(report *Report, store *engine.Engine, r engine.Record, err error) {
	row := Row{ItemID: r.Destination.ItemID, Season: r.Destination.Season, Destination: r.ServerPath}
	if errors.Is(err, errDestinationChanged) {
		if e := store.MarkStale(r.Destination); e != nil {
			report.Errors = append(report.Errors, e.Error())
		}
		row.Action = "stale"
		row.Reason = "destination ownership changed; reconcile existing audio manually"
	} else {
		row.Action = "deferred"
		row.Reason = "ownership check unavailable; existing file preserved for retry"
		report.Errors = append(report.Errors, err.Error())
	}
	report.add(row)
}

func revalidate(ctx context.Context, api *siloapi.Client, c Config, old destinations.Destination) error {
	libs, e := api.Libraries(ctx)
	if e != nil {
		return e
	}
	for _, lib := range libs {
		if lib.ID != old.LibraryID || !lib.Enabled {
			continue
		}
		inv, e := api.Inventory(ctx, lib)
		if e != nil {
			return e
		}
		for _, item := range inv.Items {
			if item.ID != old.ItemID {
				continue
			}
			var season *siloapi.Season
			if old.Season >= 0 {
				for _, v := range inv.Seasons[item.ID] {
					if v.Number == old.Season {
						season = &v
					}
				}
				if season == nil {
					return fmt.Errorf("%w: season moved or was removed", errDestinationChanged)
				}
			}
			dests, e := c.Policy.Resolve(inv, item, season)
			if e != nil {
				return fmt.Errorf("%w: %v", errDestinationChanged, e)
			}
			for _, d := range dests {
				if d == old {
					return nil
				}
			}
		}
	}
	return errDestinationChanged
}
func (s *Server) checkDiscovery(ctx context.Context, api *siloapi.Client, store *engine.Engine, library string, report *Report, explicit bool) {
	for _, r := range store.Records() {
		if r.LibraryID != library || r.Status != engine.Published {
			continue
		}
		if !explicit && (r.Attempts >= 10 || time.Since(r.LastAttempt) < time.Minute*time.Duration(1<<min(r.Attempts, 8))) {
			continue
		}
		title := strings.TrimSuffix(r.Filename, filepath.Ext(r.Filename))
		found, e := api.Discovered(ctx, r.OwnerID, r.LibraryID, title)
		if e != nil {
			report.Errors = append(report.Errors, e.Error())
			continue
		}
		if _, e = store.MarkAttempt(r.Destination, r.Filename); e != nil {
			report.Errors = append(report.Errors, e.Error())
			continue
		}
		if found {
			_, e = store.MarkDiscovered(r.Destination, r.Filename, r.OwnerID, title)
			if e != nil {
				report.Errors = append(report.Errors, e.Error())
			}
		} else if !explicit && !s.cfg.ManualRefresh && time.Since(r.Updated) > time.Minute && r.Attempts < 10 {
			if e = revalidate(ctx, api, s.cfg, r.Destination); e == nil {
				_, e = store.Reconcile(ctx, &r.Destination)
			}
			if e != nil {
				report.Errors = append(report.Errors, e.Error())
			}
		}
	}
}
func (s *Server) PollChanges(ctx context.Context, req *pluginv1.PollChangesRequest) (*pluginv1.PollChangesResponse, error) {
	if req.GetCapabilityId() != "themes" {
		return nil, errors.New("unknown scan source")
	}
	s.mu.RLock()
	if s.engine == nil {
		s.mu.RUnlock()
		return nil, errors.New("plugin is not configured")
	}
	api, store, cfg, manifestID := s.client, s.engine, s.cfg, s.manifest.GetPluginId()
	s.mu.RUnlock()
	source, e := api.ScanSource(ctx, manifestID)
	if e != nil {
		return nil, e
	}
	libs, e := api.Libraries(ctx)
	if e != nil {
		return nil, e
	}
	allowed := map[string][]string{}
	for _, l := range libs {
		if slices.Contains(cfg.LibraryIDs, l.ID) && siloapi.Eligible(l) && l.Enabled {
			allowed[l.ID] = l.Paths
		}
	}
	result, e := store.Poll(generation(cfg, source), req.GetMarker(), allowed)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	if s.engine == store {
		s.repairCount = result.RepairCount
		s.repairPaths = s.repairPaths[:0]
		for _, event := range result.Repair {
			s.repairPaths = append(s.repairPaths, event.ServerPath)
		}
		s.report.RepairCount = s.repairCount
		s.report.RepairPaths = append([]string(nil), s.repairPaths...)
		if s.repairCount > 0 && s.report.State != "prerequisite_failed" && s.report.State != "cancelled" {
			s.report.State = "needs_attention"
		}
	}
	s.mu.Unlock()
	out := &pluginv1.PollChangesResponse{NextMarker: result.NextMarker}
	for _, p := range result.Paths {
		out.Changes = append(out.Changes, &pluginv1.ScanSourceChange{SourcePath: p, Scope: pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_SUBTREE})
	}
	return out, nil
}

var page = template.Must(template.New("status").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Theme download status</title><style>body{font:16px system-ui,sans-serif;line-height:1.5;max-width:1200px;margin:32px auto;padding:0 24px;color:#20242b;background:#fafafa}h1{font-size:28px}table{border-collapse:collapse;width:100%;background:white}th,td{text-align:left;vertical-align:top;padding:12px;border-bottom:1px solid #ddd}th{font-weight:600}code{overflow-wrap:anywhere}p{max-width:75ch}small{color:#555}</style><h1>Theme download status</h1><p>State: {{.State}}. Run the Preview, Download or Reconcile task in Silo Administration to update this report.</p>{{range .Errors}}<p>{{.}}</p>{{end}}{{if .RepairCount}}<p>{{.RepairCount}} scan paths need repair and were rejected from indexing.</p>{{range .RepairPaths}}<p><code>{{.}}</code></p>{{end}}{{end}}<p>{{.Downloaded}} files downloaded in the last run. Downloaded files need a library scan and exact owner discovery before they appear as discovered.</p><table><thead><tr><th>Item / season</th><th>Theme and source</th><th>Destination</th><th>Action</th></tr></thead><tbody>{{range .Rows}}<tr><td>{{.Item}}{{if ge .Season 0}}<br><small>Season {{.Season}}</small>{{end}}</td><td>{{.Theme}}<br><small>{{.Source}}</small></td><td><code>{{.Destination}}</code></td><td>{{.Action}}<br><small>{{.Reason}}</small>{{range .Candidates}}<br><a href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.Title}}</a> <small>{{.Channel}} (score {{.Score}})</small>{{end}}</td></tr>{{end}}</tbody></table>{{if .Truncated}}<p>The report reached its 1,000-row display limit. Narrow the selected libraries to inspect the remaining items.</p>{{end}}</html>`))

func (s *Server) Handle(_ context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if req.GetMethod() != "GET" {
		return &pluginv1.HandleHTTPResponse{StatusCode: 405}, nil
	}
	s.mu.RLock()
	r := s.report
	s.mu.RUnlock()
	var b []byte
	contentType := "application/json"
	switch req.GetPath() {
	case "/status":
		var err error
		b, err = json.Marshal(r)
		if err != nil {
			return nil, err
		}
	case "/":
		var buf bytes.Buffer
		if err := page.Execute(&buf, r); err != nil {
			return nil, err
		}
		b = buf.Bytes()
		contentType = "text/html; charset=utf-8"
	default:
		return &pluginv1.HandleHTTPResponse{StatusCode: 404}, nil
	}
	return &pluginv1.HandleHTTPResponse{StatusCode: 200, Headers: map[string]string{"Content-Type": contentType, "Cache-Control": "no-store", "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'self'", "X-Content-Type-Options": "nosniff"}, Body: b}, nil
}
