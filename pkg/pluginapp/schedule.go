package pluginapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// Silo registers plugin tasks only when it starts and runs them only at
// startup unless an administrator adds a trigger, so the plugin also runs
// itself: a full run when its settings change or the last full run is a day
// old, and a run for new items after Silo scans a library.

const (
	scheduleFile     = "schedule.json"
	maxScheduleBytes = 32 << 20
	fullRunInterval  = 24 * time.Hour
	scheduleCheck    = time.Hour
)

// schedule is saved in the state directory.
type schedule struct {
	Version     int       `json:"version"`
	LastFullRun time.Time `json:"last_full_run"`
	// Settings is the fingerprint of the settings the last full run used.
	Settings string `json:"settings"`
	// Known lists the items each library had at the last run.
	Known map[string][]string `json:"known"`
}

func readSchedule(stateDir string) (schedule, error) {
	empty := schedule{Version: 1, Known: map[string][]string{}}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return empty, err
	}
	defer root.Close()
	info, err := root.Lstat(scheduleFile)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxScheduleBytes {
		return empty, errors.New("invalid run schedule")
	}
	data, err := root.ReadFile(scheduleFile)
	if err != nil {
		return empty, err
	}
	var saved schedule
	if json.Unmarshal(data, &saved) != nil || saved.Version != 1 {
		return empty, errors.New("invalid run schedule")
	}
	if saved.Known == nil {
		saved.Known = map[string][]string{}
	}
	return saved, nil
}

func writeSchedule(stateDir string, s schedule) error {
	s.Version = 1
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(data) > maxScheduleBytes {
		return errors.New("run schedule exceeds its size limit")
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".schedule-" + hex.EncodeToString(random[:]) + ".tmp"
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(name, scheduleFile)
}

// recordRun saves what a finished download run read. A full run replaces the
// known items of each library it read; a run for new items adds to them.
func recordRun(cfg Config, full bool, read map[string][]string, libs []string) error {
	saved, err := readSchedule(cfg.StateDir)
	if err != nil {
		saved = schedule{Known: map[string][]string{}}
	}
	if full {
		saved.LastFullRun = time.Now().UTC()
		saved.Settings = cfg.fingerprint()
		known := map[string][]string{}
		for _, id := range libs {
			if ids, ok := read[id]; ok {
				known[id] = ids
			} else if ids, ok := saved.Known[id]; ok {
				known[id] = ids
			}
		}
		saved.Known = known
	} else {
		for id, ids := range read {
			seen := map[string]bool{}
			for _, v := range saved.Known[id] {
				seen[v] = true
			}
			for _, v := range ids {
				if !seen[v] {
					seen[v] = true
					saved.Known[id] = append(saved.Known[id], v)
				}
			}
		}
	}
	return writeSchedule(cfg.StateDir, saved)
}

// automate checks hourly whether a full run is due, starting shortly after
// the plugin is configured. It stops when ctx ends.
func (s *Server) automate(ctx context.Context, cfg Config) {
	wait := s.startDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = scheduleCheck
		saved, err := readSchedule(cfg.StateDir)
		if err != nil {
			s.log.Warn("run schedule unreadable; starting a full run", "error", err)
		}
		if err != nil || saved.Settings != cfg.fingerprint() || time.Since(saved.LastFullRun) >= fullRunInterval {
			s.log.Info("starting automatic run")
			s.start("sync", nil)
		}
	}
}

// HandleEvent receives Silo's scan_complete events. Silo publishes one per
// scanned library folder, including the scans the plugin requests itself, so
// events only note the library; a run follows once scans settle.
func (s *Server) HandleEvent(_ context.Context, req *pluginv1.HandleEventRequest) (*pluginv1.HandleEventResponse, error) {
	if req.GetEventName() == "scan_complete" {
		if id := eventLibrary(req.GetPayload()); id != "" {
			s.log.Debug("library scan finished", "library", id)
			s.noteScan(id)
		}
	}
	return &pluginv1.HandleEventResponse{}, nil
}

// eventLibrary reads the library ID of a scan_complete event. Silo sends the
// ID as the event's bare payload, which reaches plugins as {"raw": "12"}.
func eventLibrary(p *structpb.Struct) string {
	for _, key := range []string{"raw", "library_id"} {
		switch v := p.GetFields()[key].GetKind().(type) {
		case *structpb.Value_StringValue:
			return strings.TrimSpace(v.StringValue)
		case *structpb.Value_NumberValue:
			return strconv.FormatInt(int64(v.NumberValue), 10)
		}
	}
	return ""
}

func (s *Server) noteScan(library string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.auto || s.general == nil {
		return
	}
	s.scanned[library] = true
	if s.scanTimer != nil {
		s.scanTimer.Stop()
	}
	ctx := s.jobCtx
	s.scanTimer = time.AfterFunc(s.scanDelay, func() {
		if ctx.Err() == nil {
			s.runNewItems(ctx)
		}
	})
}

// runNewItems starts a run for the items of the scanned libraries that the
// last run did not see. Without a current full run it starts a full run.
func (s *Server) runNewItems(ctx context.Context) {
	s.mu.Lock()
	scanned := s.scanned
	s.scanned = map[string]bool{}
	cfg, api := s.cfg, s.client
	s.mu.Unlock()
	if api == nil {
		return
	}
	saved, err := readSchedule(cfg.StateDir)
	if err != nil || saved.LastFullRun.IsZero() || saved.Settings != cfg.fingerprint() {
		s.start("sync", nil)
		return
	}
	libs, err := api.Libraries(ctx)
	if err != nil {
		s.log.Warn("cannot list libraries after a scan", "error", err)
		return
	}
	only := map[string][]string{}
	for _, lib := range libs {
		if !scanned[lib.ID] || !lib.Enabled || !cfg.selected(lib) {
			continue
		}
		ids, err := api.ItemIDs(ctx, lib)
		if err != nil {
			s.log.Warn("cannot list library items after a scan", "library", lib.Name, "error", err)
			continue
		}
		known := map[string]bool{}
		for _, id := range saved.Known[lib.ID] {
			known[id] = true
		}
		for _, id := range ids {
			if !known[id] {
				only[lib.ID] = append(only[lib.ID], id)
			}
		}
	}
	if len(only) > 0 {
		s.log.Info("starting a run for new items", "libraries", len(only))
		s.start("sync", only)
	}
}
