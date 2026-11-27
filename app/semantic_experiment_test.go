package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func semanticTestVector(i int) semanticVector { v:=make(semanticVector,embeddingDimension);v[i]=1;return v }

func TestEmbeddingBatchValidation(t *testing.T){
	v:=semanticTestVector(0);v[0]=3
	data,_:=json.Marshal(map[string]any{"data":[]any{map[string]any{"index":1,"embedding":semanticTestVector(1)},map[string]any{"index":0,"embedding":v}}})
	got,err:=decodeEmbeddingBatch(data,2);if err!=nil||got[0][0]!=1||got[1][1]!=1{t.Fatal(got,err)}
	for _,bad:=range [][]byte{[]byte(`{"data":[{"embedding":[1]}]}`),[]byte(`{"data":[{"index":0,"embedding":[1]}]}`)}{
		if _,err:=decodeEmbeddingBatch(bad,1);err==nil{t.Fatal("invalid batch accepted")}
	}
	v[0]=float32(math.NaN());if _,err:=normalizeSemanticVector(v);err==nil{t.Fatal("NaN accepted")}
	if _,err:=normalizeSemanticVector(make(semanticVector,embeddingDimension));err==nil{t.Fatal("zero accepted")}
}

func TestSemanticVectorStoreReuseAndClear(t *testing.T){
	run:=t.TempDir();dir,err:=requireSemanticVectorDir(run);if err!=nil{t.Fatal(err)}
	if err=saveSemanticVector(dir,"first",semanticTestVector(2));err!=nil{t.Fatal(err)}
	if v,ok:=readSemanticVector(dir,"first");!ok||v[2]!=1{t.Fatal("missing vector")}
	if _,ok:=readSemanticVector(dir,"changed");ok{t.Fatal("changed input reused")}
	other:=filepath.Join(dir,"other.txt");if err=os.WriteFile(other,[]byte("keep"),0600);err!=nil{t.Fatal(err)}
	if err=clearSemanticVectors(run);err!=nil{t.Fatal(err)}
	if _,ok:=readSemanticVector(dir,"first");ok{t.Fatal("vector retained")}
	if _,err=os.Stat(other);err!=nil{t.Fatal(err)}
}

func TestSemanticFolderRankExcludesSelfAndRepeatedIdentity(t *testing.T){
	q:=semanticMember{FileID:"self",Vectors:[]semanticVector{semanticTestVector(0)}}
	a:=semanticTestVector(0);b:=semanticTestVector(1)
	members:=[]semanticMember{
		{FileID:"self",Folder:"A",Path:"self",Vectors:[]semanticVector{a}},
		{FileID:"one",Folder:"A",Path:"one",Vectors:[]semanticVector{a}},
		{FileID:"one",Folder:"A",Path:"copy",Vectors:[]semanticVector{a}},
		{FileID:"two",Folder:"B",Path:"two",Vectors:[]semanticVector{b}},
	}
	ranked:=rankSemanticFolders(q,members)
	if len(ranked)!=2||ranked[0].Folder!="A"||ranked[0].Members!=1||ranked[0].Score!=1||ranked[1].Score!=0{t.Fatal(ranked)}
}

func TestSemanticSegmentsAndExplicitChatCommands(t *testing.T){
	segs:=semanticSegments(evidence{Title:"Title",Text:"Opening text",FollowupText:"Later text",Summary:"Title"})
	if len(segs)!=3{t.Fatal(segs)}
	for msg,action:=range map[string]string{"download embedding model":"download_embedding","compare semantic folders for file 12":"compare_semantic_folders"}{
		if p,ok:=immediateChatPlan(msg);!ok||p.Action!=action{t.Fatal(p,ok)}
	}
}
