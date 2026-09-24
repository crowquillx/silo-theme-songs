// Package engine durably publishes validated theme audio and journals scan
// requests. It requires a fresh destination proof from the caller.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowquillx/silo-theme-songs/pkg/destinations"
)

const protocol = 1
const maxAudioBytes int64 = 512 << 20
const maxRecords = 10000
const maxEvents = 10000

type Status string

const (
	Pending    Status = "pending"
	Published  Status = "published"
	Discovered Status = "discovered"
	Protected  Status = "protected"
	Stale      Status = "stale"
	Failed     Status = "failed"
)

type Record struct {
	Destination destinations.Destination `json:"destination"`
	Provider    string                   `json:"provider"`
	LibraryID   string                   `json:"library_id"`
	OwnerID     string                   `json:"owner_id"`
	LocalPath   string                   `json:"local_path"`
	ServerPath  string                   `json:"server_path"`
	Fingerprint string                   `json:"fingerprint"`
	SourceID    string                   `json:"source_id"`
	Filename    string                   `json:"filename"`
	Checksum    string                   `json:"checksum"`
	Size        int64                    `json:"size"`
	Status      Status                   `json:"status"`
	Attempts    int                      `json:"attempts"`
	LastAttempt time.Time                `json:"last_attempt"`
	JournalSeq  uint64                   `json:"journal_seq"`
	Updated     time.Time                `json:"updated"`
}

type Event struct {
	Seq        uint64    `json:"seq"`
	LibraryID  string    `json:"library_id"`
	ServerPath string    `json:"server_path"`
	Created    time.Time `json:"created"`
}

type PollResult struct {
	Events      []Event  `json:"events"`
	Paths       []string `json:"paths"`
	NextMarker  string   `json:"next_marker"`
	Ready       bool     `json:"ready"`
	Repair      []Event  `json:"repair"`
	RepairCount int      `json:"repair_count"`
}

type generationState struct {
	Baseline       uint64    `json:"baseline"`
	BaselineMarker string    `json:"baseline_marker"`
	Ready          bool      `json:"ready"`
	Acknowledged   uint64    `json:"acknowledged"`
	LastSeen       time.Time `json:"last_seen"`
}

type state struct {
	Version       int                        `json:"version"`
	NextSeq       uint64                     `json:"next_seq"`
	PrunedThrough uint64                     `json:"pruned_through"`
	Records       []Record                   `json:"records"`
	Events        []Event                    `json:"events"`
	Generations   map[string]generationState `json:"generations"`
	Issued        map[string]issuedMarker    `json:"issued"`
	Repair        []Event                    `json:"repair"`
}

type issuedMarker struct {
	Generation string      `json:"generation"`
	Cursor     uint64      `json:"cursor"`
	Created    time.Time   `json:"created"`
	Response   *PollResult `json:"response,omitempty"`
}

type Engine struct {
	mu       sync.Mutex
	root     *os.Root
	lock     *os.File
	provider string
	state    state
	closed   bool
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}\.(mp3|m4a|m4b|flac|ogg|opus|wav|aac)$`)
var providerPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
var sourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,199}$`)

func Open(stateDir, provider string) (*Engine, error) {
	if !providerPattern.MatchString(provider) {
		return nil, errors.New("invalid provider name")
	}
	if !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		return nil, errors.New("state directory must be normalized and absolute")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(stateDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid state directory")
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, err
	}
	lock, err := root.OpenFile(".silo-theme-state.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		root.Close()
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		root.Close()
		return nil, errors.New("state already open by another process")
	}
	e := &Engine{root: root, lock: lock, provider: provider}
	f, err := root.OpenFile("state.json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		e.state = state{Version: protocol, Generations: map[string]generationState{}, Issued: map[string]issuedMarker{}}
		return e, nil
	}
	if err != nil {
		e.Close()
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
		f.Close()
		e.Close()
		return nil, errors.New("invalid engine state")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		e.Close()
		return nil, err
	}
	if len(data) > 16<<20 || json.Unmarshal(data, &e.state) != nil || e.state.Version != protocol || e.state.Generations == nil || len(e.state.Records) > maxRecords || len(e.state.Events) > maxEvents || len(e.state.Issued) > maxEvents {
		e.Close()
		return nil, errors.New("invalid engine state")
	}
	if e.state.Issued == nil {
		e.state.Issued = map[string]issuedMarker{}
	}
	return e, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	_ = syscall.Flock(int(e.lock.Fd()), syscall.LOCK_UN)
	lockErr := e.lock.Close()
	rootErr := e.root.Close()
	if lockErr != nil {
		return lockErr
	}
	return rootErr
}

func (e *Engine) save() error {
	if e.closed {
		return errors.New("engine closed")
	}
	data, err := json.Marshal(e.state)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("engine state full")
	}
	name := fmt.Sprintf(".state-%d.tmp", time.Now().UnixNano())
	f, err := e.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer e.root.Remove(name)
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
	if err := e.root.Rename(name, "state.json"); err != nil {
		return err
	}
	return syncDir(e.root)
}

func syncDir(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (e *Engine) Records() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Record(nil), e.state.Records...)
}

func recordKey(r Record) string { return r.LibraryID + "\x00" + r.LocalPath + "\x00" + r.Filename }

func (e *Engine) find(key string) int {
	for i := range e.state.Records {
		if recordKey(e.state.Records[i]) == key {
			return i
		}
	}
	return -1
}

func checksumFile(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAudioBytes {
		return "", 0, errors.New("staged audio must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxAudioBytes+1))
	if err != nil || n != info.Size() {
		return "", 0, errors.New("staged audio changed")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func checksumReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, maxAudioBytes+1))
	if err != nil || n > maxAudioBytes {
		return "", n, errors.New("audio checksum failed")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func validDestination(d destinations.Destination) error {
	if d.LibraryID == "" || d.OwnerID == "" || d.ServerPath == "" || d.LocalPath == "" || d.Fingerprint == "" {
		return errors.New("incomplete destination proof")
	}
	if len(d.LocalPath) > 4096 || len(d.ServerPath) > 4096 || !filepath.IsAbs(d.LocalPath) || filepath.Clean(d.LocalPath) != d.LocalPath || !filepath.IsAbs(d.ServerPath) || filepath.Clean(d.ServerPath) != d.ServerPath {
		return errors.New("invalid destination path")
	}
	return destinations.CheckLocal(d.LocalPath)
}

func validFilename(name string) bool {
	return namePattern.MatchString(name) && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "theme.")
}

func (e *Engine) appendEvent(r Record) (uint64, error) {
	if len(e.state.Events) >= maxEvents {
		return 0, errors.New("scan journal full")
	}
	e.state.NextSeq++
	seq := e.state.NextSeq
	e.state.Events = append(e.state.Events, Event{Seq: seq, LibraryID: r.LibraryID, ServerPath: r.ServerPath, Created: time.Now().UTC()})
	return seq, nil
}

func (e *Engine) Publish(ctx context.Context, dest destinations.Destination, sourceID, filename, stagedPath string, revalidate func(context.Context) error) (Record, error) {
	if err := validDestination(dest); err != nil {
		return Record{}, err
	}
	if !validFilename(filename) || !sourcePattern.MatchString(sourceID) || revalidate == nil {
		return Record{}, errors.New("invalid publication request")
	}
	checksum, size, err := checksumFile(stagedPath)
	if err != nil {
		return Record{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, errors.New("engine closed")
	}
	owner, unlock, err := lockOwner(ctx, dest.LocalPath)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if err := checkMarker(owner, e.provider, dest.OwnerID); err != nil {
		return Record{}, err
	}
	if err := revalidate(ctx); err != nil {
		return Record{}, err
	}
	if err := validDestination(dest); err != nil {
		return Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if len(e.state.Events) >= maxEvents {
		return Record{}, errors.New("engine state full")
	}
	key := recordKey(Record{LibraryID: dest.LibraryID, LocalPath: dest.LocalPath, Filename: filename})
	replaceIndex := -1
	if i := e.find(key); i >= 0 {
		r := e.state.Records[i]
		if r.Status == Failed {
			replaceIndex = i
		} else if r.SourceID != sourceID || r.Checksum != checksum || r.Fingerprint != dest.Fingerprint || r.Status == Protected || r.Status == Stale {
			return Record{}, errors.New("existing managed file differs; operator review required")
		} else if r.Status == Published || r.Status == Discovered {
			if err := requireMarker(owner, e.provider, dest.OwnerID); err != nil {
				return Record{}, err
			}
			if err := verifyRecord(owner, r); err != nil {
				r.Status = Protected
				e.state.Records[i] = r
				_ = e.save()
				return Record{}, err
			}
			return r, nil
		} else {
			return Record{}, errors.New("pending publication requires recovery")
		}
	}
	if replaceIndex < 0 && len(e.state.Records) >= maxRecords {
		return Record{}, errors.New("engine state full")
	}
	owned := map[string]Record{}
	if len(e.state.Records) > 0 {
		for _, r := range e.state.Records {
			if r.LocalPath == dest.LocalPath && r.Status != Failed {
				if err := requireMarker(owner, e.provider, dest.OwnerID); err != nil {
					return Record{}, err
				}
				break
			}
		}
	}
	for i, r := range e.state.Records {
		if r.LocalPath != dest.LocalPath {
			continue
		}
		if r.OwnerID != dest.OwnerID || r.LibraryID != dest.LibraryID {
			return Record{}, errors.New("owner claimed by another catalog item")
		}
		switch r.Status {
		case Discovered:
			if err := verifyRecord(owner, r); err != nil {
				r.Status = Protected
				e.state.Records[i] = r
				_ = e.save()
				return Record{}, err
			}
			owned[r.Filename] = r
		case Failed:
		default:
			return Record{}, errors.New("owner has an unconfirmed or protected theme")
		}
	}
	theme, err := themeRoot(owner)
	if err != nil {
		return Record{}, err
	}
	defer theme.Close()
	if err := checkUnownedAudio(owner, theme, owned); err != nil {
		return Record{}, err
	}
	r := Record{Destination: dest, Provider: e.provider, LibraryID: dest.LibraryID, OwnerID: dest.OwnerID, LocalPath: dest.LocalPath, ServerPath: dest.ServerPath, Fingerprint: dest.Fingerprint, SourceID: sourceID, Filename: filename, Checksum: checksum, Size: size, Status: Pending, Updated: time.Now().UTC()}
	var old Record
	if replaceIndex >= 0 {
		old = e.state.Records[replaceIndex]
		e.state.Records[replaceIndex] = r
	} else {
		e.state.Records = append(e.state.Records, r)
	}
	if err := e.save(); err != nil {
		if replaceIndex >= 0 {
			e.state.Records[replaceIndex] = old
		} else {
			e.state.Records = e.state.Records[:len(e.state.Records)-1]
		}
		return Record{}, err
	}
	if err := writeMarker(owner, e.provider, dest.OwnerID); err != nil {
		return Record{}, err
	}
	if err := copyAndLink(ctx, theme, filename, stagedPath, checksum, size); err != nil {
		return Record{}, err
	}
	if err := syncDir(theme); err != nil {
		return Record{}, err
	}
	i := e.find(key)
	seq, err := e.appendEvent(r)
	if err != nil {
		return Record{}, err
	}
	r.Status = Published
	r.JournalSeq = seq
	r.Updated = time.Now().UTC()
	e.state.Records[i] = r
	if err := e.save(); err != nil {
		e.state.Records[i].Status = Pending
		e.state.Records[i].JournalSeq = 0
		e.state.Events = e.state.Events[:len(e.state.Events)-1]
		e.state.NextSeq--
		return Record{}, err
	}
	return r, nil
}
