package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunEvidenceRoundTripAndClear(t *testing.T) {
	dir:=t.TempDir()
	s:=&session{dir:dir,fileCount:1}
	st:=stamp{ID:"volume:42",Size:7,Modified:1}
	e:=evidence{Title:"Anxiety",Text:"Cognitive restructuring",FollowupText:"Grief",Photo:[]byte{1,2,3}}
	if err:=s.saveRunEvidence(0,st,e,decision{Name:"Proposed name",Kind:"article"});err!=nil{t.Fatal(err)}
	r,err:=readRunEvidence(s.evidenceDir(),0)
	if err!=nil || r.Stamp!=st || r.FileID!=evidenceFileID(st) || len(r.Evidence.Photo)!=0 || r.ProposedName!="Proposed name" {t.Fatal(r,err)}
	for q,want:=range map[string]string{"anxiety":"title","cognitive restructuring":"opening text sample","grief":"later text sample","proposed name":"","old name":"filename"}{
		if got:=runEvidenceMatch(r,"old name.pdf",q);got!=want{t.Fatalf("%s: %q want %q",q,got,want)}
	}
	other:=filepath.Join(s.evidenceDir(),"unrelated.json")
	if err=os.WriteFile(other,[]byte("keep"),0600);err!=nil{t.Fatal(err)}
	if err=s.clearRunEvidence();err!=nil{t.Fatal(err)}
	if _,err=os.Stat(evidenceRecordPath(s.evidenceDir(),0));!os.IsNotExist(err){t.Fatal("index retained",err)}
	if _,err=os.Stat(other);err!=nil{t.Fatal("unrelated file removed",err)}
}

func TestRunEvidenceQueryBounds(t *testing.T) {
	q,err:=evidenceQueries(" anxiety ; grief ; ; cognitive restructuring ")
	if err!=nil || len(q)!=3 || q[2]!="cognitive restructuring"{t.Fatal(q,err)}
	for _,bad:=range []string{" ; ",strings.Repeat("x",201),strings.Repeat("x;",17)}{
		if _,err=evidenceQueries(bad);err==nil{t.Fatal("invalid query accepted")}
	}
	p,ok:=immediateChatPlan("search indexed files for anxiety; grief")
	if !ok || p.Action!="search_run_evidence" || p.Query!="anxiety; grief"{t.Fatal(p,ok)}
}
