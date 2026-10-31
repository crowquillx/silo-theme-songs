package pluginapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
	"github.com/crowquillx/silo-theme-songs/pkg/engine"
	"github.com/crowquillx/silo-theme-songs/pkg/provider"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"github.com/hashicorp/go-hclog"
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
	State string    `json:"state"`
	Mode  string    `json:"mode"`
	At    time.Time `json:"at"`
	// NewItems marks a run limited to items added since the last run.
	NewItems   bool     `json:"new_items,omitempty"`
	Rows       []Row    `json:"rows"`
	Errors     []string `json:"errors,omitempty"`
	Notices    []string `json:"notices,omitempty"`
	Downloaded int      `json:"downloaded"`
	Truncated  bool     `json:"truncated,omitempty"`
	Message    string   `json:"message,omitempty"`
	// Summary counts every row, including rows past the display limit.
	Summary []SummaryRow `json:"summary,omitempty"`
}

type SummaryRow struct {
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count"`
}

const (
	displayRows  = 1000
	summaryLimit = 30
	maxRows      = 200000
	// maxScanRequests bounds how often the plugin asks Silo to scan one theme.
	maxScanRequests = 10
)

func (r *Report) add(row Row) {
	if len(r.Rows) >= maxRows {
		r.Truncated = true
		return
	}
	r.Rows = append(r.Rows, row)
}

func (r *Report) notice(text string) {
	if !slices.Contains(r.Notices, text) {
		r.Notices = append(r.Notices, text)
	}
}

// actionPriority orders rows so problems and planned work stay visible
// when the report is cut to its display limit.
func actionPriority(action string) int {
	switch action {
	case "download_failed", "prerequisite_failed", "needs_review", "selection_changed", "operator_review", "stale", "protected", "destination_unconfirmed", "pending":
		return 0
	case "downloaded", "would_download", "deferred":
		return 1
	case "refused":
		return 2
	case "source_unavailable", "no_theme":
		return 3
	}
	return 4
}

// finish summarizes all rows, then keeps the most important ones.
func (r *Report) finish() {
	counts := map[SummaryRow]int{}
	for _, row := range r.Rows {
		counts[SummaryRow{Action: row.Action, Reason: row.Reason}]++
	}
	r.Summary = r.Summary[:0]
	for k, n := range counts {
		k.Count = n
		r.Summary = append(r.Summary, k)
	}
	slices.SortFunc(r.Summary, func(a, b SummaryRow) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Action+a.Reason, b.Action+b.Reason)
	})
	if len(r.Summary) > summaryLimit {
		r.Summary = r.Summary[:summaryLimit]
	}
	slices.SortStableFunc(r.Rows, func(a, b Row) int { return actionPriority(a.Action) - actionPriority(b.Action) })
	if len(r.Rows) > displayRows {
		r.Rows = r.Rows[:displayRows]
		r.Truncated = true
	}
}

// Provider names identify each source's engine, owner markers and file
// names. They match the names the separate plugins used before 0.2.0.
const (
	generalName = "general"
	animeName   = "animethemes"
)

// source is one theme provider with its own state.
type source struct {
	name     string
	anime    bool
	provider Provider
	engine   *engine.Engine
}

type Server struct {
	pluginv1.UnimplementedScheduledTaskServer
	pluginv1.UnimplementedHttpRoutesServer
	pluginv1.UnimplementedEventConsumerServer
	pluginv1.UnimplementedConfigurationChecksServer
	mu           sync.RWMutex
	work         sync.Mutex
	manifest     *pluginv1.PluginManifest
	factory      Factory
	animeFactory Factory
	cfg          Config
	raw          map[string]any // settings as Silo stored them
	configErr    error
	client       *siloapi.Client
	general      *source
	anime        *source
	report       Report // the last full or requested run
	newReport    Report // the last run for new items
	progress     string
	log          hclog.Logger
	jobs         map[string]*job // background task runs by mode, guarded by mu
	jobCtx       context.Context
	cancelJobs   context.CancelFunc

	// legacyAnime reports whether the separate AnimeThemes plugin runs.
	legacyAnime func(context.Context) bool
	// auto turns on automatic runs; the CLI leaves it off.
	auto         bool
	startDelay   time.Duration
	scanDelay    time.Duration
	confirmWaits []time.Duration
	scanned      map[string]bool
	scanTimer    *time.Timer
}

type job struct {
	done   chan struct{}
	cancel context.CancelFunc
	report Report
}

// New returns a server for the general provider factory and, when anime is
// not nil, the AnimeThemes provider factory.
func New(m *pluginv1.PluginManifest, general, anime Factory) *Server {
	s := &Server{
		manifest:     m,
		factory:      general,
		animeFactory: anime,
		configErr:    errNoAPIKey,
		report:       Report{State: "not_configured", Rows: []Row{}},
		jobs:         map[string]*job{},
		log:          hclog.NewNullLogger(),
		startDelay:   20 * time.Second,
		scanDelay:    time.Minute,
		confirmWaits: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		scanned:      map[string]bool{},
		legacyAnime:  legacyAnimeEnabled,
	}
	s.jobCtx, s.cancelJobs = context.WithCancel(context.Background())
	return s
}

// stopJobs cancels background task runs so configuration changes do not wait
// behind them. Later runs use a fresh context.
func (s *Server) stopJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelJobs()
	s.jobCtx, s.cancelJobs = context.WithCancel(context.Background())
	if s.scanTimer != nil {
		s.scanTimer.Stop()
		s.scanTimer = nil
	}
}

// configure receives Silo's saved settings. Silo calls it when the plugin
// starts and restarts the plugin after the settings are saved. Settings the
// plugin cannot use leave it unconfigured with the problem on its status
// page, so the page and the settings check keep working.
func (s *Server) configure(ctx context.Context, entries []*pluginv1.ConfigEntry) error {
	var raw map[string]any
	for _, entry := range entries {
		if entry.GetKey() == "settings" {
			raw = entry.GetValue().AsMap()
		}
	}
	// ConfigureLocal logs and records its own problems.
	if cfg, err := parseSettings(raw); err == nil {
		_ = s.ConfigureLocal(ctx, cfg)
	} else {
		s.log.Warn("settings incomplete", "problem", err)
		_ = s.unconfigure(err)
	}
	s.mu.Lock()
	s.raw = raw
	s.mu.Unlock()
	return nil
}

// parseSettings decodes the settings entry Silo stores.
func parseSettings(raw map[string]any) (Config, error) {
	cfg := Defaults()
	if len(raw) == 0 {
		return cfg, errNoAPIKey
	}
	settings := maps.Clone(raw)
	if e := decodeFormText(settings); e != nil {
		return cfg, e
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return cfg, errors.New("invalid plugin settings")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(&cfg); e != nil {
		return cfg, fmt.Errorf("invalid plugin settings: %v", e)
	}
	return cfg, nil
}

// unconfigure closes the sources and records why the plugin cannot run.
func (s *Server) unconfigure(problem error) error {
	s.stopJobs()
	s.work.Lock()
	defer s.work.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.closeSources()
	s.client = nil
	s.cfg = Config{}
	s.configErr = problem
	s.report = Report{State: "not_configured", Rows: []Row{}}
	s.newReport = Report{}
	return err
}

func (s *Server) closeSources() error {
	var errs []error
	for _, src := range []*source{s.general, s.anime} {
		if src != nil && src.engine != nil {
			errs = append(errs, src.engine.Close())
		}
	}
	s.general, s.anime = nil, nil
	return errors.Join(errs...)
}

// ConfigureLocal applies decoded settings. It returns the problem when the
// settings cannot be used, which also leaves the plugin unconfigured.
func (s *Server) ConfigureLocal(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults(ctx, s.manifest.GetPluginId())
	cfg.Log = s.log
	fail := func(e error) error {
		s.log.Warn("configuration rejected", "error", e)
		_ = s.unconfigure(e)
		return e
	}
	if e := cfg.Validate(); e != nil {
		return fail(e)
	}
	client, e := siloapi.New(cfg.siloURL, cfg.APIKey, cfg.ProfileID)
	if e != nil {
		return fail(e)
	}
	s.stopJobs()
	s.work.Lock()
	s.mu.Lock()
	if s.general != nil && s.cfg.StateDir != cfg.StateDir {
		s.mu.Unlock()
		s.work.Unlock()
		return errors.New("restart the plugin before changing state_dir")
	}
	general := s.general
	if general == nil {
		store, e := engine.Open(cfg.StateDir, generalName)
		if e != nil {
			s.mu.Unlock()
			s.work.Unlock()
			return fail(e)
		}
		general = &source{name: generalName, engine: store}
	}
	p, e := s.factory(cfg)
	if e != nil {
		if s.general == nil {
			general.engine.Close()
		}
		s.mu.Unlock()
		s.work.Unlock()
		return fail(e)
	}
	general.provider = p
	// AnimeThemes opens on the next run with the new settings.
	if s.anime != nil {
		s.anime.provider = nil
	}
	s.cfg = cfg
	s.client = client
	s.general = general
	s.configErr = nil
	if s.report.State == "not_configured" {
		s.report = Report{State: "configured", Rows: []Row{}}
	}
	jobs := s.jobCtx
	s.mu.Unlock()
	s.work.Unlock()
	s.log.Info("configured", "silo_url", cfg.siloURL, "libraries", cfg.LibraryIDs, "skipped_libraries", cfg.SkipLibraryIDs, "state_dir", cfg.StateDir, "dry_run", cfg.dryRun(), "anime_themes", cfg.animeEnabled(), "primary_profile", cfg.ProfileID == "")
	if s.auto {
		go s.automate(jobs, cfg)
	}
	return nil
}

func (s *Server) Close() error {
	s.stopJobs()
	s.work.Lock()
	defer s.work.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeSources()
}

// animeSource returns AnimeThemes for a run, opening it on first use, or nil
// when it is turned off or the separate AnimeThemes plugin still runs. The
// caller holds s.work.
func (s *Server) animeSource(ctx context.Context, cfg Config, report *Report) (*source, bool) {
	if !cfg.animeEnabled() || s.animeFactory == nil {
		return nil, false
	}
	dir := filepath.Join(cfg.StateDir, "anime")
	s.mu.RLock()
	src := s.anime
	s.mu.RUnlock()
	if s.legacyAnime(ctx) {
		report.notice("AnimeThemes is now part of Theme Songs. Uninstall the separate AnimeThemes plugin so Theme Songs can manage anime themes; until then, anime libraries are left to it.")
		// Open existing AnimeThemes state so the run can leave the series
		// it manages alone.
		if src == nil {
			if _, err := os.Lstat(filepath.Join(dir, "state.json")); err == nil {
				if store, err := engine.Open(dir, animeName); err == nil {
					s.mu.Lock()
					s.anime = &source{name: animeName, anime: true, engine: store}
					s.mu.Unlock()
				}
			}
		}
		return nil, true
	}
	if src != nil && src.provider != nil {
		return src, false
	}
	if src == nil {
		if copied, err := migrateLegacyAnime(cfg.cacheDir, dir); err != nil {
			report.Errors = append(report.Errors, "AnimeThemes: "+err.Error())
			return nil, false
		} else if copied {
			s.log.Info("copied the AnimeThemes plugin's state", "state_dir", dir)
		}
		store, err := engine.Open(dir, animeName)
		if err != nil {
			report.Errors = append(report.Errors, "AnimeThemes: "+err.Error())
			return nil, false
		}
		src = &source{name: animeName, anime: true, engine: store}
	}
	animeCfg := cfg
	animeCfg.StateDir = dir
	p, err := s.animeFactory(animeCfg)
	if err != nil {
		report.Errors = append(report.Errors, "AnimeThemes: "+err.Error())
		if s.anime == nil {
			src.engine.Close()
		}
		return nil, false
	}
	src.provider = p
	s.mu.Lock()
	s.anime = src
	s.mu.Unlock()
	return src, false
}

func taskMode(key string) string {
	for _, v := range []string{"preview", "reconcile", "sync"} {
		if key == v || strings.HasSuffix(key, ":"+v) || strings.HasSuffix(key, "_"+v) || strings.HasSuffix(key, "/"+v) {
			// Reconcile was a separate task before 0.2.0; every run now
			// rechecks published themes.
			if v == "reconcile" {
				return "sync"
			}
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
	j := s.start(mode, nil)
	// Answer shortly before the RPC deadline; the run itself continues.
	var budget <-chan time.Time
	if deadline, ok := ctx.Deadline(); ok {
		t := time.NewTimer(time.Until(deadline) - time.Second)
		defer t.Stop()
		budget = t.C
	}
	report := Report{State: "running", Mode: mode, At: time.Now().UTC(), Rows: []Row{}, Message: "The task continues in the background. The Theme Songs status page shows its result."}
	select {
	case <-j.done:
		report = j.report
	case <-budget:
	case <-ctx.Done():
		// Silo cancelled the task; a deadline only ends this RPC.
		if errors.Is(ctx.Err(), context.Canceled) {
			s.forget(j)
			j.cancel()
			report.State = "cancelled"
			report.Message = ""
		}
	}
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

// start runs a task detached from the caller. Silo gives each Run call a
// 10-second control deadline, far shorter than a library read or download.
// A run already in progress for the same work is reused. only limits a run
// to listed items per library; nil reads every selected library.
func (s *Server) start(mode string, only map[string][]string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	// With dry run on, Download does what Preview does, so both share a run.
	key := mode
	if mode == "sync" && s.cfg.dryRun() {
		key = "preview"
	}
	if only != nil {
		key += ":new"
	}
	if j := s.jobs[key]; j != nil {
		s.log.Info("task joined a run already in progress", "task", mode)
		return j
	}
	ctx, cancel := context.WithCancel(s.jobCtx)
	j := &job{done: make(chan struct{}), cancel: cancel}
	s.jobs[key] = j
	go func() {
		defer cancel()
		j.report = s.Execute(ctx, mode, only)
		s.forget(j)
		close(j.done)
	}()
	return j
}

// setProgress updates the status of the running task.
func (s *Server) setProgress(format string, args ...any) {
	s.mu.Lock()
	s.progress = fmt.Sprintf(format, args...)
	s.mu.Unlock()
}

func (s *Server) forget(j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.jobs {
		if v == j {
			delete(s.jobs, k)
		}
	}
}

// run is the state of one Execute call.
type run struct {
	ctx     context.Context
	mode    string
	preview bool
	cfg     Config
	api     *siloapi.Client
	report  *Report
	// misses counts inline discovery checks that ran out of time in a row.
	misses int
	// scanUnavailable stops scan requests to a Silo without the API.
	scanUnavailable bool
	// deferred holds AnimeThemes' state while the separate AnimeThemes
	// plugin runs, so the series it already manages are left alone.
	deferred *source
}

// Execute runs a task. mode is "sync" or "preview"; only limits the run to
// listed items per library.
func (s *Server) Execute(ctx context.Context, mode string, only map[string][]string) (report Report) {
	s.work.Lock()
	defer s.work.Unlock()
	report = Report{State: "complete", Mode: mode, At: time.Now().UTC(), NewItems: only != nil, Rows: []Row{}}
	s.setProgress("%s: starting", taskName(mode))
	s.log.Info("task started", "task", mode, "new_items", only != nil)
	defer func() {
		report.finish()
		s.logResult(report)
		s.mu.Lock()
		if only != nil {
			s.newReport = report
		} else {
			s.report = report
		}
		s.progress = ""
		s.mu.Unlock()
	}()
	s.mu.RLock()
	cfg, api, general, configErr := s.cfg, s.client, s.general, s.configErr
	s.mu.RUnlock()
	if general == nil {
		report.State = "not_configured"
		if configErr == nil {
			configErr = errNoAPIKey
		}
		report.Errors = []string{configErr.Error()}
		return
	}
	r := &run{ctx: ctx, mode: mode, preview: mode == "preview" || cfg.dryRun(), api: api, report: &report}
	if e := api.ThemeCapability(ctx); e != nil {
		report.State = "prerequisite_failed"
		report.Errors = append(report.Errors, e.Error())
		r.preview = true
	}
	libs, e := api.Libraries(ctx)
	if e != nil {
		report.State = "prerequisite_failed"
		report.Errors = append(report.Errors, e.Error())
		return
	}
	settings := cfg // as configured, for the run schedule
	cfg.Policy = cfg.policy(libs)
	r.cfg = cfg
	var selected []siloapi.Library
	for _, lib := range libs {
		if cfg.selected(lib) {
			selected = append(selected, lib)
		}
	}
	for _, id := range cfg.LibraryIDs {
		if !slices.ContainsFunc(libs, func(l siloapi.Library) bool { return l.ID == id }) {
			report.Errors = append(report.Errors, "selected library is unavailable: "+id)
			report.State = "prerequisite_failed"
		}
	}
	if len(selected) == 0 {
		report.State = "prerequisite_failed"
		report.Errors = append(report.Errors, "No movie or series libraries are selected. Choose libraries on the Theme Songs page or in the plugin's settings.")
		return
	}
	anime, deferred := s.animeSource(ctx, cfg, &report)
	if deferred {
		s.mu.RLock()
		r.deferred = s.anime
		s.mu.RUnlock()
	}
	sources := []*source{general}
	if anime != nil {
		sources = append(sources, anime)
	}
	if !r.preview {
		for _, src := range sources {
			if _, e := src.engine.Recover(ctx); e != nil {
				report.Errors = append(report.Errors, e.Error())
			}
		}
	}
	read := map[string][]string{}
	// unwritable marks a run that skipped a library Silo cannot write to.
	// The fix is a setting outside Silo, so the run does not count as the
	// day's full run and the next hourly check tries again.
	unwritable := false
	for _, lib := range selected {
		if only != nil && only[lib.ID] == nil {
			continue
		}
		if !lib.Enabled {
			report.add(Row{Item: lib.Name, Action: "refused", Reason: "library is disabled"})
			continue
		}
		if deferred && slices.Contains(cfg.AnimeLibraryIDs, lib.ID) {
			report.add(Row{Item: lib.Name, Action: "excluded", Reason: "the separate AnimeThemes plugin manages this library"})
			continue
		}
		if problem := writable(cfg, lib); problem != "" {
			if r.preview {
				report.notice(problem + " Theme Songs can preview this library but cannot download to it.")
			} else {
				report.add(Row{Item: lib.Name, Action: "refused", Reason: problem})
				report.Errors = append(report.Errors, problem)
				unwritable = true
				continue
			}
		}
		s.log.Info("reading library", "library", lib.Name)
		progress := func(done, total int) {
			s.setProgress("%s: reading %s, %d of %d items", taskName(mode), lib.Name, done, total)
		}
		var inv siloapi.Inventory
		var err error
		if only != nil {
			inv, err = api.InventoryItems(ctx, lib, only[lib.ID], progress)
		} else {
			inv, err = api.InventoryProgress(ctx, lib, progress)
		}
		if err != nil {
			s.log.Warn("library read failed", "library", lib.Name, "error", err)
			report.add(Row{Item: lib.Name, Action: "refused", Reason: err.Error()})
			continue
		}
		ids := make([]string, 0, len(inv.Items))
		for _, item := range inv.Items {
			ids = append(ids, item.ID)
		}
		read[lib.ID] = ids
		if !r.preview {
			for _, src := range sources {
				s.maintain(r, src, lib.ID)
			}
		}
		for i, item := range inv.Items {
			if e := ctx.Err(); e != nil {
				report.State = "cancelled"
				return
			}
			s.setProgress("%s: checking %s, %d of %d items, %d downloaded", taskName(mode), lib.Name, i, len(inv.Items), report.Downloaded)
			s.processItem(r, inv, item, general, anime)
		}
	}
	if unwritable {
		report.Errors = append(report.Errors, "Silo's Docker Compose file mounts media read-only; remove :ro from the media volume and recreate the container.")
	}
	if mode == "sync" && report.State != "prerequisite_failed" && !(unwritable && only == nil) {
		var libIDs []string
		for _, lib := range selected {
			libIDs = append(libIDs, lib.ID)
		}
		if e := recordRun(settings, only == nil, read, libIDs); e != nil {
			report.Errors = append(report.Errors, "cannot save the run schedule: "+e.Error())
		}
	}
	if len(report.Errors) > 0 && report.State != "prerequisite_failed" && report.State != "cancelled" {
		report.State = "needs_attention"
	}
	if report.State == "complete" {
		hasReview := false
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

// writable describes why Silo cannot write to a library's folders, or
// returns "" when it can.
func writable(cfg Config, lib siloapi.Library) string {
	for _, server := range lib.Paths {
		local, err := cfg.Policy.Local(server)
		if err != nil {
			continue
		}
		switch err := syscall.Access(local, 2); {
		case errors.Is(err, syscall.EROFS):
			return fmt.Sprintf("%s is mounted read-only, so Theme Songs cannot save themes in library %s.", local, lib.Name)
		case errors.Is(err, syscall.EACCES):
			return fmt.Sprintf("Silo cannot write to %s, so Theme Songs cannot save themes in library %s.", local, lib.Name)
		}
	}
	return ""
}

// managesItem reports whether a source has a theme record for an item.
func managesItem(src *source, itemID string) bool {
	if src == nil {
		return false
	}
	for _, r := range src.engine.Records() {
		if r.Destination.ItemID == itemID && r.Status != engine.Failed {
			return true
		}
	}
	return false
}

// seasonTargets returns one target per season with episodes on disk.
func seasonTargets(inv siloapi.Inventory, item siloapi.Item) []Target {
	var targets []Target
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
	return targets
}

// processItem picks the source for an item and handles its targets. Series
// that AnimeThemes maps get season themes from it; everything else gets one
// theme from ThemerrDB. An item keeps the source that already manages it.
func (s *Server) processItem(r *run, inv siloapi.Inventory, item siloapi.Item, general, anime *source) {
	if slices.Contains(r.cfg.ExcludeItems, item.ID) || managesItem(r.deferred, item.ID) {
		return
	}
	if item.Type == "series" && anime != nil && r.cfg.animeLibrary(inv.Library.ID) && !managesItem(general, item.ID) {
		targets := append([]Target{{Item: item}}, seasonTargets(inv, item)...)
		claimed := managesItem(anime, item.ID)
		if !claimed {
			claimed = true
			if c, ok := anime.provider.(Claimer); ok {
				var err error
				if claimed, err = c.Claims(r.ctx, targets); err != nil {
					r.report.add(Row{Item: item.Title, ItemID: item.ID, Season: -1, Action: "source_unavailable", Reason: "AnimeThemes: " + err.Error()})
					return
				}
			}
		}
		if claimed {
			for _, target := range targets {
				s.processTarget(r, anime, inv, item, target)
			}
			return
		}
	}
	if item.Type == "movie" || item.Type == "series" {
		s.processTarget(r, general, inv, item, Target{Item: item})
	}
}

func (s *Server) processTarget(r *run, src *source, inv siloapi.Inventory, item siloapi.Item, target Target) {
	ctx, store, report := r.ctx, src.engine, r.report
	sn := -1
	if target.Season != nil {
		sn = target.Season.Number
	}
	row := Row{Item: item.Title, ItemID: item.ID, Season: sn}
	resolve := func() ([]destinations.Destination, bool) {
		dests, err := r.cfg.Policy.Resolve(inv, item, target.Season)
		if err != nil {
			row.Action = "refused"
			row.Reason = err.Error()
			report.add(row)
		}
		return dests, err == nil
	}
	// An anime series has a theme of its own only with a manual fallback,
	// so its folder is checked only then; its seasons have their own.
	seriesLevel := src.anime && target.Season == nil
	var dests []destinations.Destination
	if !seriesLevel {
		var ok bool
		if dests, ok = resolve(); !ok {
			return
		}
	}
	candidates, err := src.provider.Select(ctx, target)
	if err != nil {
		var review *provider.ReviewRequired
		if errors.As(err, &review) {
			row.Action = "needs_review"
			row.Reason = "Review the candidate links, then save one URL under provider.url_overrides for this item before extraction."
			row.Candidates = reviewCandidates(review.Candidates)
			report.add(row)
			return
		}
		row.Action = "source_unavailable"
		row.Reason = err.Error()
		report.add(row)
		return
	}
	if seriesLevel {
		if len(candidates) == 0 {
			return
		}
		var ok bool
		if dests, ok = resolve(); !ok {
			return
		}
	}
	if src.anime {
		reportObsolete(report, store.Records(), dests, candidates)
	}
	if len(candidates) == 0 {
		row.Action = "no_theme"
		report.add(row)
		return
	}
	for _, dest := range dests {
		for _, candidate := range candidates {
			out := row
			out.Destination = dest.ServerPath
			out.Source = candidate.Provenance
			out.Theme = candidate.Title
			if dest.Fallback {
				out.Reason = "series-owned theme, inherited by season/episodes; the series page plays it too"
			}
			if candidate.ID == "" || !audioName("theme."+strings.TrimPrefix(candidate.Extension, ".")) {
				out.Action = "source_unavailable"
				out.Reason = "candidate has no stable ID or supported audio format"
				report.add(out)
				continue
			}
			stableID := sourceID(candidate)
			if !src.anime && changedSelection(store.Records(), dest, stableID) {
				out.Action = "selection_changed"
				out.Reason = "existing managed theme preserved; review the changed source selection"
				report.add(out)
				continue
			}
			if reason := existingAudio(dest, store.Records()); reason != "" {
				out.Action = "preserved"
				out.Reason = reason
				report.add(out)
				continue
			}
			if old, ok := existing(store.Records(), dest, stableID); ok {
				out.Action = string(old.Status)
				if old.Status == engine.Published {
					out.Action = "destination_unconfirmed"
					out.Reason = "downloaded; waiting for Silo to list it"
				}
				report.add(out)
				continue
			}
			if preflight, ok := src.provider.(interface {
				Preflight(context.Context, Candidate) error
			}); ok {
				if e := preflight.Preflight(ctx, candidate); e != nil {
					out.Action = "prerequisite_failed"
					out.Reason = e.Error()
					report.add(out)
					continue
				}
			}
			if r.preview {
				out.Action = "would_download"
				report.add(out)
				continue
			}
			if r.cfg.MaxDownloads > 0 && report.Downloaded >= r.cfg.MaxDownloads {
				out.Action = "deferred"
				out.Reason = "per-run download limit; the next run continues"
				report.add(out)
				continue
			}
			if blocked(store.Records(), dest) {
				out.Action = "destination_unconfirmed"
				out.Reason = "waiting for Silo to list the previous theme in this folder"
				report.add(out)
				continue
			}
			s.setProgress("%s: downloading %s", taskName(r.mode), item.Title)
			staged, err := src.provider.Fetch(ctx, candidate)
			if err != nil {
				s.log.Warn("download failed", "item", item.Title, "season", sn, "error", err)
				out.Action = "download_failed"
				out.Reason = err.Error()
				report.add(out)
				continue
			}
			var published engine.Record
			if staged.Format != "mp3" {
				err = errors.New("provider did not produce MP3 audio")
			} else {
				output := candidate
				output.Extension = staged.Format
				published, err = store.Publish(ctx, dest, stableID, filename(src.name, output), staged.Path, func(ctx context.Context) error { return revalidate(ctx, r.api, r.cfg, dest) })
			}
			if closeErr := staged.Close(); closeErr != nil {
				report.Errors = append(report.Errors, closeErr.Error())
			}
			if err != nil {
				s.log.Warn("publication refused", "item", item.Title, "season", sn, "error", err)
				out.Action = "refused"
				out.Reason = err.Error()
				report.add(out)
				continue
			}
			s.log.Info("theme downloaded", "item", item.Title, "season", sn, "destination", dest.ServerPath, "source", candidate.Provenance)
			out.Action = "downloaded"
			report.Downloaded++
			if !s.confirm(r, src, published) {
				out.Reason = "Silo will list it after it scans the folder"
			}
			report.add(out)
		}
	}
}

// maintain rechecks a library's published themes: their files, their
// destinations, and whether Silo lists them yet.
func (s *Server) maintain(r *run, src *source, library string) {
	store, report := src.engine, r.report
	checked := map[string]bool{}
	for _, rec := range store.Records() {
		if rec.LibraryID != library || rec.Status == engine.Failed {
			continue
		}
		if !checked[rec.LocalPath] {
			checked[rec.LocalPath] = true
			if _, e := store.Check(r.ctx, rec.Destination); e != nil {
				report.Errors = append(report.Errors, e.Error())
			}
		}
		switch {
		case rec.Status == engine.Stale:
			// A destination that resolves exactly again is restored.
			if revalidate(r.ctx, r.api, r.cfg, rec.Destination) == nil {
				if _, e := store.Restore(r.ctx, rec.Destination); e != nil {
					report.Errors = append(report.Errors, e.Error())
				}
			}
		case rec.Destination.Fallback && rec.Status != engine.Protected:
			if e := revalidate(r.ctx, r.api, r.cfg, rec.Destination); e != nil {
				recordRevalidationFailure(report, store, rec, e)
			}
		}
	}
	s.checkDiscovery(r, src, library)
}

func discoveryBackoff(attempts int) time.Duration {
	return time.Minute << min(attempts, 8)
}

// checkDiscovery marks published themes Silo now lists, and asks Silo again
// to scan the others, with growing waits between checks.
func (s *Server) checkDiscovery(r *run, src *source, library string) {
	for _, rec := range src.engine.Records() {
		if rec.LibraryID != library || rec.Status != engine.Published {
			continue
		}
		if time.Since(rec.LastAttempt) < discoveryBackoff(rec.Attempts) {
			continue
		}
		found, err := s.discovered(r, src, rec)
		if err != nil {
			r.report.Errors = append(r.report.Errors, err.Error())
			continue
		}
		if found {
			continue
		}
		scan := rec.ScanRequests < maxScanRequests && !r.scanUnavailable
		if rec, err = src.engine.MarkAttempt(rec.Destination, rec.Filename, scan); err != nil {
			r.report.Errors = append(r.report.Errors, err.Error())
			continue
		}
		if scan {
			s.requestScan(r, rec)
		} else if rec.ScanRequests >= maxScanRequests {
			r.report.add(Row{ItemID: rec.Destination.ItemID, Season: rec.Destination.Season, Destination: rec.ServerPath, Theme: rec.Filename, Action: "destination_unconfirmed", Reason: "Silo has not listed this theme after repeated scans; check that the folder belongs to this item"})
		}
	}
}

// discovered checks whether Silo lists a published theme under its exact
// owner, and records it when it does.
func (s *Server) discovered(r *run, src *source, rec engine.Record) (bool, error) {
	title := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
	found, err := r.api.Discovered(r.ctx, rec.OwnerID, rec.LibraryID, title)
	if err != nil || !found {
		return false, err
	}
	_, err = src.engine.MarkDiscovered(rec.Destination, rec.Filename, rec.OwnerID, title)
	return err == nil, err
}

// requestScan asks Silo to scan a published theme file. It reports whether
// Silo accepted the request.
func (s *Server) requestScan(r *run, rec engine.Record) bool {
	if r.scanUnavailable {
		return false
	}
	err := r.api.RequestScan(r.ctx, rec.LibraryID, filepath.Join(rec.ServerPath, "theme-music", rec.Filename))
	switch {
	case err == nil:
		return true
	case errors.Is(err, siloapi.ErrScanUnavailable):
		r.scanUnavailable = true
		r.report.notice("This Silo version cannot scan single folders through its API, so new themes appear after its next library scan.")
	default:
		r.report.notice("Silo refused a scan request (" + err.Error() + "), so some new themes appear after its next library scan.")
	}
	return false
}

// confirm asks Silo to scan a theme just published and waits briefly for
// Silo to list it, so a folder can take its next theme in the same run.
// After three slow scans in a row, later themes in the run skip the wait.
func (s *Server) confirm(r *run, src *source, rec engine.Record) bool {
	rec, err := src.engine.MarkAttempt(rec.Destination, rec.Filename, !r.scanUnavailable)
	if err != nil || !s.requestScan(r, rec) || r.misses >= 3 {
		return false
	}
	for _, wait := range s.confirmWaits {
		select {
		case <-r.ctx.Done():
			return false
		case <-time.After(wait):
		}
		found, err := s.discovered(r, src, rec)
		if err != nil {
			return false
		}
		if found {
			r.misses = 0
			return true
		}
	}
	r.misses++
	return false
}

func taskName(mode string) string {
	if mode == "sync" {
		return "Download"
	}
	return "Preview"
}

// logResult writes one line per run plus one per kind of problem, so a large
// library does not produce a line per item.
func (s *Server) logResult(r Report) {
	s.log.Info("task finished", "task", r.Mode, "new_items", r.NewItems, "state", r.State, "downloaded", r.Downloaded, "rows", len(r.Rows), "seconds", int(time.Since(r.At).Seconds()))
	for _, e := range r.Errors {
		s.log.Warn("task error", "task", r.Mode, "error", e)
	}
	for _, row := range r.Summary {
		if actionPriority(row.Action) == 0 || row.Action == "refused" {
			s.log.Warn("items need attention", "task", r.Mode, "action", row.Action, "reason", row.Reason, "count", row.Count)
		}
	}
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
		row.Reason = "destination ownership changed; the file is kept, review it"
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
	c.Policy = c.policy(libs)
	for _, lib := range libs {
		if lib.ID != old.LibraryID || !lib.Enabled {
			continue
		}
		inv, e := api.ItemInventory(ctx, lib, old.ItemID)
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
