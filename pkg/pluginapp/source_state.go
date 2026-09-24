package pluginapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"syscall"
)

const sourceStateFile = ".silo-theme-source-state.json"

type sourceState struct {
	Version              int    `json:"version"`
	ReconciledGeneration string `json:"reconciled_generation"`
}

func readReconciledGeneration(stateDir string) (string, error) {
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat(sourceStateFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", errors.New("invalid source reconciliation state")
	}
	data, err := root.ReadFile(sourceStateFile)
	if err != nil {
		return "", err
	}
	var saved sourceState
	if json.Unmarshal(data, &saved) != nil || saved.Version != 1 || len(saved.ReconciledGeneration) != 64 {
		return "", errors.New("invalid source reconciliation state")
	}
	return saved.ReconciledGeneration, nil
}

func writeReconciledGeneration(stateDir, generation string) error {
	if len(generation) != 64 {
		return errors.New("invalid source generation")
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.Marshal(sourceState{Version: 1, ReconciledGeneration: generation})
	if err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".silo-theme-source-" + hex.EncodeToString(random[:]) + ".tmp"
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
	if err := root.Rename(name, sourceStateFile); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
