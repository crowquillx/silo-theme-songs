package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
)

const maxPollPaths = 100
const maxPollBytes = 48 << 10

func (e *Engine) issue(generation string, cursor uint64) (string, error) {
	if len(e.state.Issued) >= maxEvents {
		return "", errors.New("issued scan markers full")
	}
	for range 3 {
		var token [24]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", err
		}
		marker := hex.EncodeToString(token[:])
		if _, exists := e.state.Issued[marker]; exists {
			continue
		}
		e.state.Issued[marker] = issuedMarker{Generation: generation, Cursor: cursor, Created: time.Now().UTC()}
		return marker, nil
	}
	return "", errors.New("scan marker collision")
}

func (e *Engine) Ready(generation string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && generation != "" && e.state.Generations[generation].Ready
}

// Poll returns SUBTREE owner paths. An empty marker establishes one durable
// baseline per source generation; only a later echo marks the source ready.
// A returned event is not evidence that the host indexed its audio.
func (e *Engine) Poll(generation, marker string, allowed map[string][]string) (PollResult, error) {
	if generation == "" || len(generation) > 100 || strings.ContainsRune(generation, 0) {
		return PollResult{}, errors.New("invalid source generation")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return PollResult{}, errors.New("engine closed")
	}
	g, exists := e.state.Generations[generation]
	if marker == "" {
		if !exists {
			g = generationState{Baseline: e.state.NextSeq, Acknowledged: e.state.NextSeq, LastSeen: time.Now().UTC()}
			issued, err := e.issue(generation, g.Baseline)
			if err != nil {
				return PollResult{}, err
			}
			g.BaselineMarker = issued
			e.state.Generations[generation] = g
			if err := e.save(); err != nil {
				delete(e.state.Generations, generation)
				delete(e.state.Issued, issued)
				return PollResult{}, err
			}
		} else if g.BaselineMarker == "" {
			issued, err := e.issue(generation, g.Baseline)
			if err != nil {
				return PollResult{}, err
			}
			g.BaselineMarker = issued
			e.state.Generations[generation] = g
			if err := e.save(); err != nil {
				delete(e.state.Issued, issued)
				return PollResult{}, err
			}
		}
		return PollResult{NextMarker: g.BaselineMarker, Ready: g.Ready}, nil
	}
	if !exists {
		return PollResult{}, errors.New("unknown scan generation")
	}
	issued, ok := e.state.Issued[marker]
	if !ok || issued.Generation != generation {
		return PollResult{}, errors.New("scan marker was not issued for this generation")
	}
	if issued.Response != nil {
		for _, event := range issued.Response.Events {
			if !allowedPath(event, allowed) {
				return PollResult{}, errors.New("issued scan path is no longer in an enabled library root")
			}
		}
		return clonePoll(*issued.Response), nil
	}
	oldGeneration := g
	oldRepairLen := len(e.state.Repair)
	oldEvents := append([]Event(nil), e.state.Events...)
	oldPruned := e.state.PrunedThrough
	oldIssued := make(map[string]issuedMarker, len(e.state.Issued))
	for token, record := range e.state.Issued {
		oldIssued[token] = record
	}
	restore := func() {
		e.state.Generations[generation] = oldGeneration
		e.state.Repair = e.state.Repair[:oldRepairLen]
		e.state.Events = oldEvents
		e.state.PrunedThrough = oldPruned
		e.state.Issued = oldIssued
	}
	cursor := issued.Cursor
	if cursor < g.Baseline || cursor < e.state.PrunedThrough || cursor > e.state.NextSeq {
		return PollResult{}, errors.New("invalid or stale scan marker")
	}
	changed := false
	if !g.Ready {
		g.Ready = true
		changed = true
	}
	if cursor > g.Acknowledged {
		g.Acknowledged = cursor
		changed = true
	}
	g.LastSeen = time.Now().UTC()
	changed = true
	result := PollResult{Ready: true}
	seen := map[string]bool{}
	last := cursor
	bytesUsed := 0
	for _, event := range e.state.Events {
		if event.Seq <= cursor {
			continue
		}
		if !allowedPath(event, allowed) {
			if !hasRepair(e.state.Repair, event.Seq) {
				e.state.Repair = append(e.state.Repair, event)
				changed = true
			}
			last = event.Seq
			continue
		}
		key := event.LibraryID + "\x00" + event.ServerPath
		if !seen[key] {
			if len(result.Paths) == maxPollPaths || bytesUsed+len(event.ServerPath)+len(event.LibraryID)+100 > maxPollBytes {
				break
			}
			seen[key] = true
			result.Paths = append(result.Paths, event.ServerPath)
			result.Events = append(result.Events, event)
			bytesUsed += len(event.ServerPath) + len(event.LibraryID) + 100
		}
		last = event.Seq
	}
	result.RepairCount = len(e.state.Repair)
	repairBytes := 0
	for _, event := range e.state.Repair {
		if len(result.Repair) == 20 {
			break
		}
		if repairBytes+len(event.ServerPath)+len(event.LibraryID)+100 > 8<<10 {
			break
		}
		result.Repair = append(result.Repair, event)
		repairBytes += len(event.ServerPath) + len(event.LibraryID) + 100
	}
	next, err := e.issue(generation, last)
	if err != nil {
		restore()
		return PollResult{}, err
	}
	result.NextMarker = next
	issued.Response = &result
	e.state.Issued[marker] = issued
	e.state.Generations[generation] = g
	if e.pruneAcknowledged() {
		changed = true
	}
	e.pruneIssued()
	if changed {
		if err := e.save(); err != nil {
			restore()
			return PollResult{}, err
		}
	}
	return clonePoll(result), nil
}

func clonePoll(r PollResult) PollResult {
	r.Paths = append([]string(nil), r.Paths...)
	r.Events = append([]Event(nil), r.Events...)
	r.Repair = append([]Event(nil), r.Repair...)
	return r
}

func (e *Engine) pruneIssued() {
	cutoff := time.Now().Add(-24 * time.Hour)
	for token, issued := range e.state.Issued {
		g := e.state.Generations[issued.Generation]
		if token != g.BaselineMarker && issued.Created.Before(cutoff) {
			delete(e.state.Issued, token)
		}
	}
}

// Keep acknowledged events for 24 hours so an older marker can be retried.
// Generations idle for more than seven days stop holding journal retention.
func (e *Engine) pruneAcknowledged() bool {
	now := time.Now()
	minAck := e.state.NextSeq
	for _, g := range e.state.Generations {
		if now.Sub(g.LastSeen) <= 7*24*time.Hour && g.Acknowledged < minAck {
			minAck = g.Acknowledged
		}
	}
	cut := 0
	for cut < len(e.state.Events) {
		ev := e.state.Events[cut]
		if ev.Seq > minAck || now.Sub(ev.Created) < 24*time.Hour {
			break
		}
		e.state.PrunedThrough = ev.Seq
		cut++
	}
	if cut == 0 {
		return false
	}
	e.state.Events = append([]Event(nil), e.state.Events[cut:]...)
	return true
}

func hasRepair(repair []Event, seq uint64) bool {
	for _, event := range repair {
		if event.Seq == seq {
			return true
		}
	}
	return false
}

func allowedPath(event Event, allowed map[string][]string) bool {
	if len(event.ServerPath) > 8192 || !filepath.IsAbs(event.ServerPath) || filepath.Clean(event.ServerPath) != event.ServerPath {
		return false
	}
	for _, root := range allowed[event.LibraryID] {
		if filepath.IsAbs(root) && filepath.Clean(root) == root && destinations.Within(root, event.ServerPath) {
			return true
		}
	}
	return false
}

func (e *Engine) MarkAttempt(dest destinations.Destination, filename string) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := e.find(recordKey(Record{LibraryID: dest.LibraryID, LocalPath: dest.LocalPath, Filename: filename}))
	if i < 0 {
		return Record{}, errors.New("unknown theme")
	}
	r := e.state.Records[i]
	if r.Status != Published {
		return Record{}, errors.New("theme is not awaiting discovery")
	}
	if r.Attempts >= 10 {
		return Record{}, errors.New("discovery attempts exhausted")
	}
	r.Attempts++
	r.LastAttempt = time.Now().UTC()
	e.state.Records[i] = r
	if err := e.save(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// MarkDiscovered requires the catalog's observed owner and filename-derived
// title, not an Autoscan marker. A checksum mismatch protects the file.
func (e *Engine) MarkDiscovered(dest destinations.Destination, filename, observedOwnerID, observedTitle string) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, errors.New("engine closed")
	}
	i := e.find(recordKey(Record{LibraryID: dest.LibraryID, LocalPath: dest.LocalPath, Filename: filename}))
	if i < 0 {
		return Record{}, errors.New("unknown theme")
	}
	r := e.state.Records[i]
	if r.OwnerID != observedOwnerID || strings.TrimSuffix(filename, filepath.Ext(filename)) != observedTitle {
		return Record{}, errors.New("theme discovery owner or title mismatch")
	}
	if r.Status != Published && r.Status != Discovered {
		return Record{}, errors.New("theme is not awaiting discovery")
	}
	lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, unlock, err := lockOwner(lockCtx, r.LocalPath)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if err := requireMarker(owner, e.provider, r.OwnerID); err != nil {
		return Record{}, err
	}
	if err := verifyRecord(owner, r); err != nil {
		r.Status = Protected
		r.Updated = time.Now().UTC()
		e.state.Records[i] = r
		_ = e.save()
		return Record{}, err
	}
	if r.Status == Discovered {
		return r, nil
	}
	r.Status = Discovered
	r.Attempts++
	r.LastAttempt = time.Now().UTC()
	r.Updated = r.LastAttempt
	e.state.Records[i] = r
	if err := e.save(); err != nil {
		return Record{}, err
	}
	return r, nil
}

func (e *Engine) MarkStale(dest destinations.Destination) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("engine closed")
	}
	changed := false
	for i, r := range e.state.Records {
		if r.LibraryID == dest.LibraryID && r.LocalPath == dest.LocalPath && r.OwnerID == dest.OwnerID && r.Status != Protected {
			r.Status = Stale
			r.Updated = time.Now().UTC()
			e.state.Records[i] = r
			changed = true
		}
	}
	if !changed {
		return errors.New("no matching managed owner")
	}
	return e.save()
}

// Check verifies all managed files for an owner without emitting scan events.
// Normal scheduled runs can use it even when no download is needed.
func (e *Engine) Check(ctx context.Context, dest destinations.Destination) ([]Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("engine closed")
	}
	hasRecords := false
	for _, r := range e.state.Records {
		if r.LibraryID == dest.LibraryID && r.LocalPath == dest.LocalPath && r.OwnerID == dest.OwnerID && r.Status != Failed {
			hasRecords = true
			break
		}
	}
	if !hasRecords {
		return nil, nil
	}
	owner, unlock, err := lockOwner(ctx, dest.LocalPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := requireMarker(owner, e.provider, dest.OwnerID); err != nil {
		return nil, err
	}
	var result []Record
	for i, r := range e.state.Records {
		if r.LibraryID != dest.LibraryID || r.LocalPath != dest.LocalPath || r.OwnerID != dest.OwnerID {
			continue
		}
		if r.Status != Failed && r.Status != Pending {
			if err := verifyRecord(owner, r); err != nil {
				r.Status = Protected
				r.Updated = time.Now().UTC()
				e.state.Records[i] = r
				if saveErr := e.save(); saveErr != nil {
					return result, saveErr
				}
				return result, err
			}
		}
		result = append(result, r)
	}
	return result, nil
}

// Recover resolves pending intents. It emits one journal event only when the
// intended file and checksum match. Missing finals become retryable failures.
func (e *Engine) Recover(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0, errors.New("engine closed")
	}
	count := 0
	for i, r := range e.state.Records {
		if r.Status != Pending {
			continue
		}
		if err := ctx.Err(); err != nil {
			return count, err
		}
		owner, unlock, err := lockOwner(ctx, r.LocalPath)
		if err != nil {
			return count, err
		}
		err = verifyRecord(owner, r)
		if err == nil {
			err = requireMarker(owner, e.provider, r.OwnerID)
		}
		unlock()
		if errors.Is(err, os.ErrNotExist) {
			r.Status = Failed
		} else if err != nil {
			r.Status = Protected
		} else {
			seq, appendErr := e.appendEvent(r)
			if appendErr != nil {
				return count, appendErr
			}
			r.Status = Published
			r.JournalSeq = seq
			count++
		}
		r.Updated = time.Now().UTC()
		e.state.Records[i] = r
		if saveErr := e.save(); saveErr != nil {
			return count, saveErr
		}
	}
	return count, nil
}

// Reconcile explicitly re-emits existing managed files after a source reset.
// A stale file may be restored only by an explicit, exactly matching owner
// proof; a nil owner never clears stale or protected records.
func (e *Engine) Reconcile(ctx context.Context, owner *destinations.Destination) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0, errors.New("engine closed")
	}
	count := 0
	for i := range e.state.Records {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		r := e.state.Records[i]
		if owner != nil && (r.LibraryID != owner.LibraryID || r.LocalPath != owner.LocalPath || r.OwnerID != owner.OwnerID) {
			continue
		}
		restoreStale := r.Status == Stale && owner != nil && *owner == r.Destination
		if r.Status != Published && r.Status != Discovered && !restoreStale {
			continue
		}
		locked, unlock, err := lockOwner(ctx, r.LocalPath)
		if err != nil {
			return count, err
		}
		err = requireMarker(locked, e.provider, r.OwnerID)
		if err == nil {
			err = verifyRecord(locked, r)
		}
		unlock()
		if err != nil {
			r.Status = Protected
			r.Updated = time.Now().UTC()
			e.state.Records[i] = r
			if saveErr := e.save(); saveErr != nil {
				return count, saveErr
			}
			continue
		}
		seq, err := e.appendEvent(r)
		if err != nil {
			return count, err
		}
		r.JournalSeq = seq
		if restoreStale {
			r.Status = Published
			r.Attempts = 0
			r.LastAttempt = time.Time{}
		}
		r.Updated = time.Now().UTC()
		e.state.Records[i] = r
		if err := e.save(); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func verifyRecord(owner *os.Root, r Record) error {
	theme, err := owner.OpenRoot("theme-music")
	if err != nil {
		return err
	}
	defer theme.Close()
	info, err := theme.Lstat(r.Filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != r.Size {
		return fmt.Errorf("managed theme changed: %s", r.Filename)
	}
	f, err := theme.Open(r.Filename)
	if err != nil {
		return err
	}
	defer f.Close()
	hash, n, err := checksumReader(f)
	if err != nil || n != r.Size || hash != r.Checksum {
		return errors.New("managed theme checksum changed")
	}
	return nil
}
