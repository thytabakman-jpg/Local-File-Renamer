package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type metadataTransaction struct {
	Stage, Backup              string
	Before, After, BackupStamp stamp
}

func (s *session) metadataApproved(index int) bool {
	for _, plan := range s.metadataPlans {
		if plan.Index == index {
			return true
		}
	}
	return false
}

func (s *session) metadataNext() error {
	plan := s.metadataPlans[s.metadataIndex]
	path := s.currentPath(plan.Path, plan.Index)
	if err := safeParent(s.manifest.Root, path); err != nil {
		return s.metadataSkip(plan.Index, path, err.Error())
	}
	before, err := fingerprint(path)
	if err != nil || before != plan.Stamp {
		return s.metadataSkip(plan.Index, path, "file changed after the scan")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Size() > maxMetadataFileSize || info.Mode().Perm()&0200 == 0 {
		return s.metadataSkip(plan.Index, path, "file unavailable, read-only or over the 128 MiB repair limit")
	}
	backup := filepath.Join(s.dir, fmt.Sprintf("original-%d", plan.Index))
	original, err := os.CreateTemp(s.dir, "metadata-backup-")
	if err != nil {
		return s.metadataSkip(plan.Index, path, err.Error())
	}
	defer os.Remove(original.Name())
	if err = copyToFile(path, original); err != nil {
		return s.metadataSkip(plan.Index, path, err.Error())
	}
	if err = os.Rename(original.Name(), backup); err != nil {
		return s.metadataSkip(plan.Index, path, err.Error())
	}
	backupStamp, err := fingerprint(backup)
	if err != nil {
		return s.metadataSkip(plan.Index, path, err.Error())
	}
	r := record{Type: "metadata-intent", Index: plan.Index, Path: path}
	t, err := s.stageMetadata(path, backup, plan.Stamp, func(stage *os.File) error {
		err := rewriteMetadata(backup, extension(path), stage, plan.Metadata)
		return errors.Join(err, stage.Close())
	})
	if err != nil {
		_ = os.Remove(backup)
		return s.metadataSkip(plan.Index, path, err.Error())
	}
	t.BackupStamp = backupStamp
	r.Transaction = t
	if err = s.append(r); err != nil {
		return err
	}
	return s.finishPending()
}

func (s *session) metadataSkip(index int, path, reason string) error {
	return s.append(record{Type: "metadata-result", Index: index, Path: path, Status: "Metadata skipped", Note: "Metadata repair: " + reason})
}

func copyToFile(path string, out *os.File) (err error) {
	defer func() { err = errors.Join(err, out.Close()) }()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = io.Copy(out, f); err != nil {
		return err
	}
	return out.Sync()
}

func (s *session) stageMetadata(path, backup string, expected stamp, prepare func(*os.File) error) (*metadataTransaction, error) {
	if err := safeParent(s.manifest.Root, path); err != nil {
		return nil, err
	}
	before, err := fingerprint(path)
	if err != nil || before != expected {
		return nil, errors.New("the file changed after the scan or repair")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".local-file-renamer-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = prepare(f); err != nil {
		return nil, err
	}
	if err = os.Chmod(f.Name(), info.Mode().Perm()); err != nil {
		return nil, err
	}
	after, err := fingerprint(f.Name())
	if err != nil {
		return nil, err
	}
	ok = true
	return &metadataTransaction{Stage: f.Name(), Backup: backup, Before: before, After: after}, nil
}

func (s *session) finishMetadata(r record) error {
	t := r.Transaction
	if err := safeParent(s.manifest.Root, r.Path); err != nil {
		return err
	}
	if filepath.Dir(t.Stage) != filepath.Dir(r.Path) {
		return errors.New("metadata staging file is outside the original folder")
	}
	undo := r.Type == "metadata-undo-intent"
	current, err := fingerprint(r.Path)
	switch {
	case err == nil && current == t.After:
		// The app stopped after replacement and before the journal result.
	case err != nil || current != t.Before:
		return fmt.Errorf("could not verify file before metadata change: %s", r.Path)
	default:
		stage, err := fingerprint(t.Stage)
		if err != nil || stage != t.After {
			return fmt.Errorf("could not verify staged metadata change for %s", r.Path)
		}
		if err = replaceExisting(t.Stage, r.Path); err != nil {
			if undo {
				return err
			}
			stillOriginal, _ := fingerprint(r.Path)
			if stillOriginal != t.Before {
				return err
			}
			if err := s.metadataSkip(r.Index, r.Path, err.Error()); err != nil {
				return err
			}
			_ = os.Remove(t.Stage)
			_ = os.Remove(t.Backup)
			return nil
		}
	}
	current, err = fingerprint(r.Path)
	if err != nil || current != t.After {
		return fmt.Errorf("could not verify file after metadata change: %s", r.Path)
	}
	if undo {
		err := s.append(record{Type: "metadata-undo-result", Index: r.Index, Stamp: current})
		if err == nil {
			_ = os.Remove(t.Backup)
		}
		return err
	}
	return s.append(record{Type: "metadata-result", Index: r.Index, Path: r.Path, Transaction: t, Stamp: current})
}

func (s *session) undoMetadata() error {
	edit := s.repaired[len(s.repaired)-s.metadataUndone-1]
	path := s.currentPath(edit.Path, edit.Index)
	backup := edit.Transaction.Backup
	if b, err := fingerprint(backup); err != nil || b != edit.Transaction.BackupStamp {
		return fmt.Errorf("could not verify the original metadata backup for %s", edit.Path)
	}
	t, err := s.stageMetadata(path, backup, edit.Stamp, func(stage *os.File) error {
		if err := copyToFile(backup, stage); err != nil {
			return err
		}
		modified := time.Unix(0, edit.Transaction.Before.Modified)
		return os.Chtimes(stage.Name(), modified, modified)
	})
	if err != nil {
		return fmt.Errorf("restore metadata for %s: %w", edit.Path, err)
	}
	if err = s.append(record{Type: "metadata-undo-intent", Index: edit.Index, Path: path, Transaction: t}); err != nil {
		return err
	}
	return s.finishPending()
}
