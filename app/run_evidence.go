package main

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
	"strconv"
	"strings"
)

const runEvidenceVersion = 1
const runEvidenceLimit = 2 << 20
const runEvidenceMaxHits = 30

type runEvidenceRecord struct {
	Version, Index int
	FileID string
	Stamp stamp
	Evidence evidence
	ProposedName, Classification, Outcome, Reason string
}

func evidenceFileID(st stamp) string {
	if st.ID == "" { return "" }
	hash := sha256.Sum256([]byte("filesystem-v1:"+st.ID))
	return hex.EncodeToString(hash[:])
}

func (s *session) evidenceDir() string { return filepath.Join(s.dir, "searchable-evidence") }
func evidenceRecordPath(dir string, i int) string { return filepath.Join(dir, strconv.Itoa(i)+".json") }

func (s *session) saveRunEvidence(i int, st stamp, e evidence, d decision) error {
	if st.ID == "" || st.Dir || i < 0 || i >= s.fileCount { return nil }
	dir := s.evidenceDir()
	if err := checkLocalDataDirectory(dir,true); err != nil { return err }
	e.Photo = nil
	// The existing extraction caps are preserved; never imply full coverage.
	e.Text, e.FollowupText = chars(e.Text,100000), chars(e.FollowupText,100000)
	rec := runEvidenceRecord{Version:runEvidenceVersion,Index:i,FileID:evidenceFileID(st),Stamp:st,Evidence:e,
		ProposedName:d.Name,Classification:d.Kind,Outcome:d.Outcome,Reason:d.Reason}
	return atomicJSON(evidenceRecordPath(dir,i),rec)
}

func readRunEvidence(dir string, i int) (runEvidenceRecord,error) {
	var rec runEvidenceRecord
	path := evidenceRecordPath(dir,i)
	info, err := os.Lstat(path)
	if err != nil { return rec,err }
	if !info.Mode().IsRegular() || linked(info) || info.Size() > runEvidenceLimit { return rec,errors.New("Invalid searchable evidence entry") }
	f, err := os.Open(path)
	if err != nil { return rec,err }
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f,runEvidenceLimit)).Decode(&rec)
	if err != nil { return rec,err }
	if rec.Version != runEvidenceVersion || rec.Index != i || rec.Stamp.Dir || rec.FileID == "" || rec.FileID != evidenceFileID(rec.Stamp) {
		return rec,errors.New("Searchable evidence identity mismatch")
	}
	return rec,nil
}

func (s *session) clearRunEvidence() error {
	return errors.Join(s.clearEvidenceRecords(), clearSemanticVectors(s.dir))
}

func (s *session) clearEvidenceRecords() error {
	dir := s.evidenceDir()
	if err := checkLocalDataDirectory(dir,false); err != nil {
		if errors.Is(err,os.ErrNotExist) { return nil }; return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil { return err }
	var failures []error
	for _, item := range entries {
		n := item.Name()
		if !strings.HasSuffix(n,".json") { continue }
		i, err := strconv.Atoi(strings.TrimSuffix(n,".json"))
		if err != nil || i < 0 || strconv.Itoa(i)+".json" != n { continue }
		info,err := item.Info()
		if err != nil { failures=append(failures,err); continue }
		if !info.Mode().IsRegular() || linked(info) { continue }
		if err=os.Remove(filepath.Join(dir,n)); err != nil { failures=append(failures,err) }
	}
	return errors.Join(failures...)
}

func evidenceQueries(query string) ([]string,error) {
	var queries []string
	for _, q := range strings.Split(query,";") {
		q=strings.TrimSpace(q)
		if q == "" { continue }
		if len(q)>200 { return nil,errors.New("Each search phrase must be at most 200 bytes") }
		queries=append(queries,q)
	}
	if len(queries)==0 || len(queries)>16 { return nil,errors.New("Enter one to sixteen phrases separated by semicolons") }
	return queries,nil
}

// Return the exact field where a literal phrase is present. No semantic or
// full-document absence claim is made.
func runEvidenceMatch(rec runEvidenceRecord, path, query string) string {
	e:=rec.Evidence
	query=strings.ToLower(query)
	for _, field := range []struct{ name,value string }{
		{"filename",filepath.Base(path)},{"title",e.Title},{"author",e.Author},{"year",e.Year},
		{"venue",e.Venue},{"publisher",e.Publisher},{"summary",e.Summary},
		{"opening text sample",e.Text},{"later text sample",e.FollowupText},
	} {
		if strings.Contains(strings.ToLower(field.value),query) { return field.name }
	}
	return ""
}

// Shared admission boundary for lexical and semantic consumers. The source
// fingerprint, journal location and selected-root checks remain authoritative.
func (s *session) currentRunEvidence(i int) (runEvidenceRecord, string, string) {
	if i < 0 || i >= s.fileCount || i >= len(s.manifest.Items) || i >= len(s.plans) || s.manifest.Items[i].Skip {
		return runEvidenceRecord{}, "", "missing"
	}
	rec,err:=readRunEvidence(s.evidenceDir(),i)
	if errors.Is(err,os.ErrNotExist) { return rec,"","missing" }
	if err!=nil { return rec,"","unreadable" }
	path:=s.currentPath(s.manifest.Items[i].Path,i)
	if safeParent(s.manifest.Root,path)!=nil { return rec,path,"stale" }
	st,err:=fingerprint(path)
	if err!=nil || st!=rec.Stamp { return rec,path,"stale" }
	return rec,path,""
}

func (a *application) searchRunEvidence(ctx context.Context, job, query string) (chatReply,error) {
	queries,err:=evidenceQueries(query)
	if err!=nil { return chatReply{},err }
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.v.Busy || a.v.Closing { return chatReply{},errors.New("Pause the current scan before searching saved evidence") }
	s,owned,err:=a.bridgeRunLocked(job,true)
	if err!=nil { return chatReply{},err }
	if owned { defer s.close() }
	dir:=s.evidenceDir()
	if err=checkLocalDataDirectory(dir,false); err!=nil {
		if errors.Is(err,os.ErrNotExist) { return chatReply{Message:"This run has no searchable evidence. Enable saved evidence in Settings and start a new Rename or Sorter scan."},nil }
		return chatReply{},err
	}
	groups:=make([]chatResultGroup,len(queries))
	counts:=make([]int,len(queries))
	indexed,missing,stale,unreadable:=0,0,0,0
	for i:=0;i<s.fileCount;i++ {
		if err:=ctx.Err();err!=nil{return chatReply{},err}
		rec,path,state:=s.currentRunEvidence(i)
		switch state {
		case "missing": missing++; continue
		case "stale": stale++; continue
		case "unreadable": unreadable++; continue
		}
		indexed++
		relative,err:=filepath.Rel(s.manifest.Root,path)
		if err!=nil { relative=filepath.Base(path) }
		for j,q:=range queries {
			field:=runEvidenceMatch(rec,path,q)
			if field=="" { continue }
			counts[j]++
			if len(groups[j].Files)<runEvidenceMaxHits {
				groups[j].Files=append(groups[j].Files,fmt.Sprintf("%s | %s | matched %s",relative,rec.FileID[:12],field))
			}
		}
	}
	for i,q:=range queries {
		groups[i].Label=fmt.Sprintf("%s: %d matches (showing %d)",q,counts[i],len(groups[i].Files))
	}
	return chatReply{Message:fmt.Sprintf("Saved-run search: %d current indexed files; %d missing or unassessed; %d stale or moved outside the run; %d unreadable records. Searches use sampled evidence and current filenames, not full-document semantic analysis.",indexed,missing,stale,unreadable),Groups:groups},nil
}
