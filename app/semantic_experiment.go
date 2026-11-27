package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The only owner of experiment sequencing. Model-slot acquisition precedes
// the application lock; neither vector storage nor folder scoring acquires it.
func (a *application) semanticTask(request context.Context, job string, work func(context.Context,scanSettings,*session)(chatReply,error)) (result chatReply, err error) {
	select {case a.chat.slot<-struct{}{}:default:return result,errors.New("The local model is busy")}
	a.mu.Lock()
	if a.v.Busy || a.v.Closing || job!="" && (a.s==nil || a.v.Job!=job) {
		a.mu.Unlock();<-a.chat.slot
		return result,errors.New("Select the saved run again and pause any active operation")
	}
	opts,s:=a.activeSettings(),a.s
	ctx:=a.begin("semantic-experiment")
	cancel:=a.cancel
	a.mu.Unlock()
	stop:=context.AfterFunc(request,cancel)
	defer func(){stop();cancel();<-a.chat.slot;a.finish(err)}()
	a.chat.closeModel()
	return work(ctx,opts,s)
}

func (a *application) downloadEmbedding(ctx context.Context) (chatReply,error) {
	return a.semanticTask(ctx,"",func(ctx context.Context,_ scanSettings,_ *session)(chatReply,error){
		if _,err:=verifyEmbeddingModel(ctx,a.modelDir);err==nil{return chatReply{Message:"The pinned BGE embedding model is installed and verified."},nil}
		if err:=ctx.Err();err!=nil{return chatReply{},err}
		if err:=os.MkdirAll(a.modelDir,0700);err!=nil{return chatReply{},err}
		if err:=fetchModel(ctx,a.modelDir,embeddingBase,embeddingResource,0,func(int64){});err!=nil{return chatReply{},err}
		if _,err:=verifyEmbeddingModel(ctx,a.modelDir);err!=nil{return chatReply{},err}
		return chatReply{Message:"The 67.3 MB BGE embedding model was downloaded and verified. Real inference has not been tested yet."},nil
	})
}

type semanticDocument struct {
	member semanticMember
	index int
	stamp stamp
	segments []string
}

func (a *application) compareSemanticFolders(ctx context.Context,job string,index int)(chatReply,error){
	if job==""{return chatReply{},errors.New("Select a saved run first")}
	return a.semanticTask(ctx,job,func(ctx context.Context,opts scanSettings,s *session)(chatReply,error){
		if err:=checkLocalDataDirectory(s.evidenceDir(),false);err!=nil{return chatReply{},errors.New("This run has no accessible searchable evidence; enable saved evidence and scan first")}
		target,path,state:=s.currentRunEvidence(index)
		if state!=""{return chatReply{},fmt.Errorf("Selected file evidence is %s",state)}
		query:=semanticDocument{member:semanticMember{FileID:target.FileID,Path:path,Folder:filepath.Dir(path)},index:index,stamp:target.Stamp,segments:semanticSegments(target.Evidence)}
		if len(query.segments)==0{return chatReply{},errors.New("Selected file has no usable text sample")}
		modelPath,err:=verifyEmbeddingModel(ctx,a.modelDir);if err!=nil{return chatReply{},err}
		dir,err:=requireSemanticVectorDir(s.dir);if err!=nil{return chatReply{},err}
		missing,stale,unreadable,empty,excluded,unsampled:=0,0,0,0,0,0
		var candidates []semanticDocument
		for i:=0;i<s.fileCount;i++{
			if err:=ctx.Err();err!=nil{return chatReply{},err}
			rec,path,state:=s.currentRunEvidence(i)
			switch state{case "missing":missing++;continue;case "stale":stale++;continue;case "unreadable":unreadable++;continue}
			if rec.FileID==query.member.FileID{excluded++;continue}
			segments:=semanticSegments(rec.Evidence);if len(segments)==0{empty++;continue}
			candidates=append(candidates,semanticDocument{member:semanticMember{FileID:rec.FileID,Path:path,Folder:filepath.Dir(path)},index:i,stamp:rec.Stamp,segments:segments})
		}
		sort.Slice(candidates,func(i,j int)bool{return candidates[i].member.Path<candidates[j].member.Path})
		docs:=[]semanticDocument{query}
		counts:=map[string]int{};seen:=map[string]bool{};sampleSignatures:=map[string]bool{}
		for _,d:=range candidates{
			k:=d.member.Folder+"\x00"+d.member.FileID
			// Identical sampled inputs cannot inflate one folder's profile. This
			// does not establish whole-file equivalence or authorize recycling.
			sampleKey:=d.member.Folder+"\x00"+semanticInputKey(strings.Join(d.segments,"\x00"))
			if seen[k] || sampleSignatures[sampleKey] || counts[d.member.Folder]>=maxFolderSamples{unsampled++;continue}
			seen[k]=true;sampleSignatures[sampleKey]=true;counts[d.member.Folder]++;docs=append(docs,d)
		}
		if len(docs)<2{return chatReply{},errors.New("No distinct folder member has usable current evidence")}
		vectors:=map[string]semanticVector{}
		var pending []string
		seenInputs:=map[string]bool{}
		for _,d:=range docs{for _,text:=range d.segments{
			key:=semanticInputKey(text);if seenInputs[key]{continue};seenInputs[key]=true
			if v,ok:=readSemanticVector(dir,text);ok{vectors[key]=v}else{pending=append(pending,text)}
		}}
		cached:=len(vectors)
		if len(pending)>0{
			m,err:=startEmbeddingModel(ctx,a.backend,modelPath,filepath.Join(s.dir,"embedding-model.log"),opts);if err!=nil{return chatReply{},err}
			defer m.Close()
			for i:=0;i<len(pending);i+=embeddingBatchSize{
				if err:=ctx.Err();err!=nil{return chatReply{},err}
				texts:=pending[i:min(i+embeddingBatchSize,len(pending))]
				batch,err:=m.embedSemanticBatch(ctx,texts);if err!=nil{return chatReply{},err}
				for j,text:=range texts{if err=saveSemanticVector(dir,text,batch[j]);err!=nil{return chatReply{},err};vectors[semanticInputKey(text)]=batch[j]}
			}
		}
		// Re-admit sources after inference; external changes cannot silently
		// enter the resulting frozen comparison. No live sorter state is modified.
		members:=make([]semanticMember,0,len(docs)-1)
		for i,d:=range docs{
			if err:=ctx.Err();err!=nil{return chatReply{},err}
			rec,path,state:=s.currentRunEvidence(d.index)
			if state!="" || path!=d.member.Path || rec.Stamp!=d.stamp{
				if i==0{return chatReply{},errors.New("Selected file changed during semantic comparison")}
				stale++;continue
			}
			for _,text:=range d.segments{d.member.Vectors=append(d.member.Vectors,vectors[semanticInputKey(text)])}
			if i==0{query.member=d.member}else{members=append(members,d.member)}
		}
		scores:=rankSemanticFolders(query.member,members)
		if len(scores)==0{return chatReply{},errors.New("No current folder profiles remain")}
		rel:=func(path string)string{p,err:=filepath.Rel(s.manifest.Root,path);if err!=nil{return filepath.Base(path)};return p}
		groups:=make([]chatResultGroup,0,min(10,len(scores)))
		for _,score:=range scores[:min(10,len(scores))]{
			label:=fmt.Sprintf("%s: %.3f similarity (%d distinct members)",rel(score.Folder),score.Score,score.Members)
			if score.Folder==query.member.Folder{label+=" [current folder]"}
			group:=chatResultGroup{Label:label}
			for _,w:=range score.Witnesses{group.Files=append(group.Files,rel(w))}
			groups=append(groups,group)
		}
		return chatReply{Message:fmt.Sprintf("Experimental comparison for %s (file %d): %d folders; %d admitted representative members; %d reused and %d newly embedded text segments. Excluded: %d missing, %d stale, %d unreadable, %d without text, %d self/identity matches, %d duplicate or unsampled members. Showing up to 10 folders. No move was proposed or made. Similarity is not destination correctness.",rel(query.member.Path),index,len(scores),len(members),cached,len(pending),missing,stale,unreadable,empty,excluded,unsampled),Groups:groups},nil
	})
}
