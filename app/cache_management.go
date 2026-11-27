package main

import (
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "os"
 "path/filepath"
 "strings"
 "time"
)

const defaultCacheHours = 90 * 24

type cachePolicy struct { RetentionHours int `json:"retention_hours"` }
type cacheStats struct { Entries int `json:"entries"`; Bytes int64 `json:"bytes"`; RetentionHours int `json:"retention_hours"` }

func (p cachePolicy) valid() bool { return p.RetentionHours >= 0 && p.RetentionHours <= 365*24 }

func readCachePolicy(home string) (cachePolicy, error) {
 p := cachePolicy{RetentionHours: defaultCacheHours}
 data, err := os.ReadFile(filepath.Join(home, "cache-policy.json"))
 if errors.Is(err, os.ErrNotExist) { return p, nil }
 if err != nil { return p, err }
 if err = json.Unmarshal(data, &p); err != nil { return p, err }
 if !p.valid() { return p, errors.New("Invalid saved cache retention") }
 return p, nil
}

func checkLocalDataDirectory(dir string, create bool) error {
	if create {
		if err := os.Mkdir(dir,0700); err != nil && !errors.Is(err,os.ErrExist) { return err }
	}
	info, err := os.Lstat(dir)
	if err != nil { return err }
	if !info.IsDir() || linked(info) { return errors.New("Unsafe local data directory") }
	return nil
}

func isDigestJSONFile(name string) bool {
 if len(name) != 69 || !strings.HasSuffix(name, ".json") { return false }
 _, err := hex.DecodeString(name[:64]); return err == nil
}

// Shared low-level storage helper: only regular files with a digest JSON key are eligible.
func walkDigestJSONFiles(dir string, visit func(string, os.FileInfo) error) error {
 root, err := os.Lstat(dir)
 if errors.Is(err, os.ErrNotExist) { return nil }
 if err != nil { return err }
 if !root.IsDir() || linked(root) { return errors.New("Unsafe digest JSON directory") }
 entries, err := os.ReadDir(dir)
 if err != nil { return err }
 var failures []error
 for _, entry := range entries {
  if !isDigestJSONFile(entry.Name()) { continue }
  info, err := entry.Info()
  if err != nil { failures = append(failures, err); continue }
  if !info.Mode().IsRegular() || linked(info) { continue }
  if err = visit(filepath.Join(dir, entry.Name()), info); err != nil { failures = append(failures, err) }
 }
 return errors.Join(failures...)
}

func assessmentCacheStats(dir string, p cachePolicy) (cacheStats, error) {
 stats := cacheStats{RetentionHours:p.RetentionHours}
 err := walkDigestJSONFiles(dir, func(_ string, info os.FileInfo) error { stats.Entries++; stats.Bytes += info.Size(); return nil })
 return stats, err
}

func pruneAssessmentCache(dir string, p cachePolicy, now time.Time) error {
 if !p.valid() { return errors.New("Invalid cache retention") }
 if p.RetentionHours == 0 { return nil }
 cutoff := now.Add(-time.Duration(p.RetentionHours)*time.Hour)
 return walkDigestJSONFiles(dir, func(path string, info os.FileInfo) error { if info.ModTime().Before(cutoff) { return os.Remove(path) }; return nil })
}

func pruneConfiguredCache(home string) error {
 p, err := readCachePolicy(home)
 if err != nil { return err }
 return pruneAssessmentCache(filepath.Join(home,"assessment-cache"),p,time.Now())
}

func clearAssessmentCache(dir string, assessmentsOnly bool) error {
 return walkDigestJSONFiles(dir, func(path string, info os.FileInfo) error {
  if !assessmentsOnly { return os.Remove(path) }
  if info.Size() > 2<<20 { return fmt.Errorf("Cache entry too large for selective clearing: %s",filepath.Base(path)) }
  data, err := os.ReadFile(path); if err != nil { return err }
  var fields map[string]json.RawMessage
  if err = json.Unmarshal(data,&fields); err != nil { return err }
  changed := false
  for _, key := range []string{"Model","Move","AssessmentVersion","Kind","Rules"} { if _, ok := fields[key]; ok { delete(fields,key); changed = true } }
  if !changed { return nil }
  if err = atomicJSON(path,fields); err != nil { return err }
  return os.Chtimes(path,info.ModTime(),info.ModTime())
 })
}
