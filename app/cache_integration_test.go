package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyCacheMigratesAndSurvivesMove(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	for _, dir := range []string{cache, source, target} {
		if err := os.Mkdir(dir, 0700); err != nil { t.Fatal(err) }
	}
	first := filepath.Join(source, "paper.txt")
	second := filepath.Join(target, "paper.txt")
	if err := os.WriteFile(first, []byte("Original document contents"), 0600); err != nil { t.Fatal(err) }
	st, err := fingerprint(first)
	if err != nil { t.Fatal(err) }
	opts := defaultSettings()
	legacy := cachePath(cache, first, false)
	record := assessmentCache{Stamp:st,EvidenceVersion:evidenceVersion,Evidence:evidence{Text:"cached proof"},
		EvidenceConfig:opts.Evidence,DecodeMemory:opts.Resources.DecodeMemoryMiB,AIConfig:opts.AI}
	if err = atomicJSON(legacy,record); err != nil { t.Fatal(err) }
	old := time.Now().Add(-2*time.Hour)
	if err = os.Chtimes(legacy,old,old); err != nil { t.Fatal(err) }
	key := stableCachePath(cache,first,false,st)
	pre := prepareEvidence(context.Background(),first,filepath.Base(first),key,st,false,opts)
	if !pre.Reused || pre.Evidence.Text != "cached proof" { t.Fatalf("legacy cache not reused: %+v",pre) }
	if _, ok := readCache(key,st); !ok { t.Fatal("stable entry missing") }
	legacyInfo, err := os.Stat(legacy); if err != nil { t.Fatal(err) }
	keyInfo, err := os.Stat(key); if err != nil { t.Fatal(err) }
	if !keyInfo.ModTime().Equal(legacyInfo.ModTime()) { t.Fatal("migration reset cache retention") }
	if err = os.Rename(first,second); err != nil { t.Fatal(err) }
	moved, err := fingerprint(second); if err != nil { t.Fatal(err) }
	pre = prepareEvidence(context.Background(),second,filepath.Base(second),stableCachePath(cache,second,false,moved),moved,false,opts)
	if !pre.Reused || pre.Evidence.Text != "cached proof" { t.Fatalf("moved cache not reused: %+v",pre) }
	if err = os.WriteFile(second,[]byte("Changed file with a different length and different contents"),0600); err != nil { t.Fatal(err) }
	changed, err := fingerprint(second); if err != nil { t.Fatal(err) }
	pre = prepareEvidence(context.Background(),second,filepath.Base(second),stableCachePath(cache,second,false,changed),changed,false,opts)
	if pre.Reused || !strings.Contains(pre.Evidence.Text,"Changed file") { t.Fatalf("stale evidence reused: %+v",pre) }
}

func TestCacheHTTPControls(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home,"assessment-cache")
	if err := os.Mkdir(dir,0700); err != nil { t.Fatal(err) }
	a := &application{home:home}
	mux := http.NewServeMux()
	a.registerCacheRoutes(mux,func(w http.ResponseWriter,r *http.Request) bool {
		if r.Header.Get("X-Test-Auth") == "yes" { return true }
		http.Error(w,"Forbidden",http.StatusForbidden); return false
	})
	entry := filepath.Join(dir,strings.Repeat("a",64)+".json")
	source := filepath.Join(home,"source.txt")
	if err := os.WriteFile(entry,[]byte(`{"Evidence":{"Text":"keep"},"Model":{"Label":"topic"}}`),0600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(source,[]byte("source"),0600); err != nil { t.Fatal(err) }
	request := func(method, body string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method,"/api/cache",strings.NewReader(body))
		req.Header.Set("Content-Type","application/json")
		if authorized { req.Header.Set("X-Test-Auth","yes") }
		w := httptest.NewRecorder(); mux.ServeHTTP(w,req); return w
	}
	if w := request("POST",`{"action":"clear-all"}`,false); w.Code != http.StatusForbidden { t.Fatal(w.Code) }
	if w := request("POST",`{"action":"set-retention","retention_hours":24}`,true); w.Code != 200 { t.Fatal(w.Code,w.Body.String()) }
	w := request("GET","",true)
	var stats cacheStats
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(),&stats) != nil || stats.Entries != 1 || stats.RetentionHours != 24 { t.Fatal(w.Code,w.Body.String()) }
	a.v.Busy = true
	if w = request("POST",`{"action":"clear-all"}`,true); w.Code != 409 { t.Fatal(w.Code) }
	a.v.Busy = false
	if w = request("POST",`{"action":"clear-assessments"}`,true); w.Code != 200 { t.Fatal(w.Code,w.Body.String()) }
	data, err := os.ReadFile(entry)
	if err != nil || !strings.Contains(string(data),"Evidence") || strings.Contains(string(data),"Model") { t.Fatal(string(data),err) }
	if w = request("POST",`{"action":"clear-all"}`,true); w.Code != 200 { t.Fatal(w.Code,w.Body.String()) }
	if _, err = os.Stat(entry); !os.IsNotExist(err) { t.Fatal("cache entry not removed",err) }
	if _, err = os.Stat(source); err != nil { t.Fatal("source removed",err) }
}
