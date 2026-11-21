package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveDeletesManagedThemeAndForgetsIt(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	if err := e.Remove(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.LocalPath, "theme-music", r.Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file kept: %v", err)
	}
	if len(e.Records()) != 0 {
		t.Fatalf("record kept: %+v", e.Records())
	}
	// The folder takes a new theme afterwards.
	if _, err := e.PublishAudio(context.Background(), d, "tmdb:123", "loudness:-16", "general-themerrdb-123-title-lufs16.mp3", stage, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := e.Records(); len(got) != 1 || got[0].Audio != "loudness:-16" {
		t.Fatalf("records=%+v", got)
	}
}

func TestRemoveKeepsChangedFile(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := publish(t, e, d, stage)
	path := filepath.Join(d.LocalPath, "theme-music", r.Filename)
	if err := os.WriteFile(path, []byte("edited by someone"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(context.Background(), r); err == nil {
		t.Fatal("changed file removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("changed file deleted: %v", err)
	}
	if got := e.Records(); len(got) != 1 || got[0].Status != Protected {
		t.Fatalf("records=%+v", got)
	}
}

func TestRecoverFinishesInterruptedRemoval(t *testing.T) {
	d, stage, stateDir := fixture(t)
	e, err := Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	r := publish(t, e, d, stage)
	// Remove stopped after recording its intent, before the file went.
	e.state.Records[0].Status = Removing
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, err = Open(stateDir, "general")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.LocalPath, "theme-music", r.Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file kept: %v", err)
	}
	if len(e.Records()) != 0 {
		t.Fatalf("record kept: %+v", e.Records())
	}
}
