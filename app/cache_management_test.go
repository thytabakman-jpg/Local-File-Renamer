package main

import (
 "encoding/json"
 "os"
 "path/filepath"
 "strings"
 "testing"
 "time"
)

func TestCacheManagementRetentionAndClear(t *testing.T) {
 dir := t.TempDir()
 now := time.Now()
 old := filepath.Join(dir,strings.Repeat("a",64)+".json")
 recent := filepath.Join(dir,strings.Repeat("b",64)+".json")
 other := filepath.Join(dir,"unrelated.json")
 for _, p := range []string{old,recent,other} { if err := os.WriteFile(p,[]byte(`{"Evidence":{"Text":"retained"},"Model":{"Label":"topic"},"Move":{"Version":"2"},"UnknownField":true}`),0600); err != nil { t.Fatal(err) } }
 if err := os.Chtimes(old,now.Add(-25*time.Hour),now.Add(-25*time.Hour)); err != nil { t.Fatal(err) }
 if err := pruneAssessmentCache(dir,cachePolicy{0},now); err != nil { t.Fatal(err) }
 if _, err := os.Stat(old); err != nil { t.Fatal("unlimited retention pruned old file",err) }
 if err := pruneAssessmentCache(dir,cachePolicy{24},now); err != nil { t.Fatal(err) }
 if _, err := os.Stat(old); !os.IsNotExist(err) { t.Fatal("expired entry retained",err) }
 stats, err := assessmentCacheStats(dir,cachePolicy{24}); if err != nil || stats.Entries != 1 || stats.Bytes <= 0 { t.Fatal(stats,err) }
 if err = clearAssessmentCache(dir,true); err != nil { t.Fatal(err) }
 data, err := os.ReadFile(recent); if err != nil { t.Fatal(err) }
 var fields map[string]json.RawMessage; if err = json.Unmarshal(data,&fields); err != nil { t.Fatal(err) }
 if fields["Evidence"] == nil || fields["UnknownField"] == nil || fields["Model"] != nil || fields["Move"] != nil { t.Fatal(string(data)) }
 if err = clearAssessmentCache(dir,false); err != nil { t.Fatal(err) }
 if _, err = os.Stat(recent); !os.IsNotExist(err) { t.Fatal("cache not cleared",err) }
 if _, err = os.Stat(other); err != nil { t.Fatal("unrelated file removed",err) }
}

func TestCachePolicyDefaultsAndValidation(t *testing.T) {
 home := t.TempDir(); p,err := readCachePolicy(home); if err != nil || p.RetentionHours != 2160 { t.Fatal(p,err) }
 if err = os.WriteFile(filepath.Join(home,"cache-policy.json"),[]byte(`{"retention_hours":24}`),0600); err != nil { t.Fatal(err) }
 p,err = readCachePolicy(home); if err != nil || p.RetentionHours != 24 { t.Fatal(p,err) }
 if (cachePolicy{-1}).valid() || (cachePolicy{8761}).valid() { t.Fatal("invalid retention accepted") }
}

func TestCacheKeySurvivesMoveButNotRename(t *testing.T) {
 dir := t.TempDir()
 a := filepath.Join(dir,"source","paper.pdf")
 b := filepath.Join(dir,"target","paper.pdf")
 st := stamp{ID:"volume:file",Size:50,Modified:10}
 key := stableCachePath(dir,a,false,st)
 if key != stableCachePath(dir,b,false,st) { t.Fatal("folder move changed cache key") }
 if key == stableCachePath(dir,filepath.Join(dir,"target","renamed.pdf"),false,st) { t.Fatal("rename reused filename-dependent assessment") }
 if key == stableCachePath(dir,b,true,st) { t.Fatal("vision and text keys collide") }
 st.ID="other"
 if key == stableCachePath(dir,b,false,st) { t.Fatal("distinct file IDs collide") }
 if stableCachePath("",a,false,st) != "" { t.Fatal("disabled cache is enabled") }
}
