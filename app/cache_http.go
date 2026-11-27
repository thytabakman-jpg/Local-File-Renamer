package main

import (
 "encoding/json"
 "net/http"
 "path/filepath"
 "strings"
)

func (a *application) registerCacheRoutes(mux *http.ServeMux, auth func(http.ResponseWriter,*http.Request) bool) {
 mux.HandleFunc("GET /api/cache",func(w http.ResponseWriter,r *http.Request) {
  if !auth(w,r) { return }
  a.mu.Lock(); defer a.mu.Unlock()
  p,err := readCachePolicy(a.home)
  if err != nil { http.Error(w,err.Error(),409); return }
  stats,err := assessmentCacheStats(filepath.Join(a.home,"assessment-cache"),p)
  if err != nil { http.Error(w,err.Error(),409); return }
  w.Header().Set("Cache-Control","no-store")
  reply(w,stats)
 })
 mux.HandleFunc("POST /api/cache",func(w http.ResponseWriter,r *http.Request) {
  if !auth(w,r) { return }
  if !strings.HasPrefix(r.Header.Get("Content-Type"),"application/json") { http.Error(w,"JSON action required",415); return }
  var q struct { Action string `json:"action"`; RetentionHours *int `json:"retention_hours"`; Job string `json:"job"` }
  if err := json.NewDecoder(http.MaxBytesReader(w,r.Body,4096)).Decode(&q); err != nil { http.Error(w,"Invalid cache action",400); return }
  a.mu.Lock(); defer a.mu.Unlock()
  if a.v.Busy || a.v.Closing { http.Error(w,"Pause the active run before managing the cache",409); return }
  dir := filepath.Join(a.home,"assessment-cache")
  var err error
  switch q.Action {
  case "set-retention":
   if q.RetentionHours == nil || !(cachePolicy{*q.RetentionHours}).valid() { http.Error(w,"Retention must be 0 to 8760 hours (0 keeps entries indefinitely)",400); return }
   err = atomicJSON(filepath.Join(a.home,"cache-policy.json"),cachePolicy{*q.RetentionHours})
  case "clear-all":
   err = clearAssessmentCache(dir,false)
  case "clear-assessments":
   err = clearAssessmentCache(dir,true)
  case "clear-run-evidence":
   if a.s == nil || q.Job == "" || q.Job != a.v.Job { http.Error(w,"Select the saved run again before clearing its evidence",409); return }
   err = a.s.clearRunEvidence()
  default:
   http.Error(w,"Unknown cache action",400); return
  }
  if err != nil { http.Error(w,err.Error(),409); return }
  p,err := readCachePolicy(a.home)
  if err != nil { http.Error(w,err.Error(),409); return }
  stats,err := assessmentCacheStats(dir,p)
  if err != nil { http.Error(w,err.Error(),409); return }
  reply(w,stats)
 })
}
