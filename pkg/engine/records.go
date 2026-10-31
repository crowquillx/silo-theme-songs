package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
)

// maxAttempts bounds the attempt counter; callers decide when to give up.
const maxAttempts = 1000

// MarkAttempt records a discovery check of a published theme, and whether
// the check asked Silo to scan it.
func (e *Engine) MarkAttempt(dest destinations.Destination, filename string, scanRequested bool) (Record, error) {
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
	r.Attempts = min(r.Attempts+1, maxAttempts)
	if scanRequested {
		r.ScanRequests = min(r.ScanRequests+1, maxAttempts)
	}
	r.LastAttempt = time.Now().UTC()
	e.state.Records[i] = r
	if err := e.save(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// MarkDiscovered requires the catalog's observed owner and filename-derived
// title. A checksum mismatch protects the file.
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
	r.Attempts = min(r.Attempts+1, maxAttempts)
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

// Check verifies all managed files for an owner. Every run uses it, including
// runs that download nothing.
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

// Recover resolves pending intents. A pending file whose content matches
// becomes published; a missing one becomes a retryable failure.
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
			r.Status = Published
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

// Restore returns stale records to published once the caller proves the
// exact destination again. The owner marker and file content must still
// match; otherwise the records become protected. Protected records stay
// protected.
func (e *Engine) Restore(ctx context.Context, owner destinations.Destination) (int, error) {
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
		if r.Status != Stale || r.Destination != owner {
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
		r.Updated = time.Now().UTC()
		if err != nil {
			r.Status = Protected
		} else {
			r.Status = Published
			r.Attempts = 0
			r.LastAttempt = time.Time{}
			count++
		}
		e.state.Records[i] = r
		if err := e.save(); err != nil {
			return count, err
		}
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
