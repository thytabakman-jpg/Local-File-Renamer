package main

import (
	"sort"
)

type semanticMember struct {
	FileID, Path, Folder string
	Vectors []semanticVector
}

type semanticFolderScore struct {
	Folder string
	Score float64
	Members int
	Witnesses []string
}

// One file contributes at most one score. Multiple segments preserve distinct
// topics; a single anomalously similar segment cannot represent the whole query.
func semanticMemberScore(query, member []semanticVector) float64 {
	if len(query)==0 || len(member)==0{return -1}
	var total float64
	for _,q:=range query {
		best:=-1.0
		for _,v:=range member {
			var score float64
			for i,x:=range q {score+=float64(x)*float64(v[i])}
			if score>best{best=score}
		}
		total+=best
	}
	return total/float64(len(query))
}

// Pure computation: callers admit current evidence, normalize vectors and freeze
// representative members. Never generates or authorizes a destination.
func rankSemanticFolders(query semanticMember,members []semanticMember) []semanticFolderScore {
	type hit struct{ path string; score float64 }
	groups:=map[string][]hit{}
	seen:=map[string]bool{}
	for _,member:=range members {
		if member.FileID=="" || member.FileID==query.FileID || len(member.Vectors)==0{continue}
		key:=member.Folder+"\x00"+member.FileID
		if seen[key]{continue};seen[key]=true
		groups[member.Folder]=append(groups[member.Folder],hit{member.Path,semanticMemberScore(query.Vectors,member.Vectors)})
	}
	out:=make([]semanticFolderScore,0,len(groups))
	for folder,hits:=range groups {
		sort.Slice(hits,func(i,j int)bool{if hits[i].score!=hits[j].score{return hits[i].score>hits[j].score};return hits[i].path<hits[j].path})
		s:=semanticFolderScore{Folder:folder,Members:len(hits)}
		n:=min(3,len(hits))
		for _,h:=range hits[:n]{s.Score+=h.score;s.Witnesses=append(s.Witnesses,h.path)}
		s.Score/=float64(n)
		out=append(out,s)
	}
	sort.Slice(out,func(i,j int)bool{if out[i].Score!=out[j].Score{return out[i].Score>out[j].Score};return out[i].Folder<out[j].Folder})
	return out
}

