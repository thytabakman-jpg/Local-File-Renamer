package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const embeddingDimension = 384
const embeddingBatchSize = 8
// Bump this protocol when changing the bundled llama.cpp engine or preprocessing.
const embeddingProtocol = "bge-f16-cls-segments-v1-llama-b12588"
const embeddingBase = "https://huggingface.co/CompendiumLabs/bge-small-en-v1.5-gguf/resolve/d32f8c040ea3b516330eeb75b72bcc2d3a780ab7/"

var embeddingResource = resource{"bge-small-en-v1.5-f16.gguf", "f0b2fef971e8366438bfd2d9aefea1b0115919389448806d290237f638bae999", 67308128}

type semanticVector []float32

func normalizeSemanticVector(v semanticVector) (semanticVector, error) {
	if len(v) != embeddingDimension { return nil, errors.New("Unexpected embedding dimension") }
	var norm float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f,0) { return nil,errors.New("Non-finite embedding") }
		norm += f*f
	}
	if norm == 0 { return nil,errors.New("Zero embedding") }
	out := make(semanticVector,len(v))
	for i,x := range v { out[i]=float32(float64(x)/math.Sqrt(norm)) }
	return out,nil
}

func decodeEmbeddingBatch(data []byte, count int) ([]semanticVector,error) {
	var result struct {
		Data []struct {
			Index *int `json:"index"`
			Embedding semanticVector `json:"embedding"`
		} `json:"data"`
	}
	if count<1 || count>embeddingBatchSize || json.Unmarshal(data,&result)!=nil || len(result.Data)!=count {
		return nil,errors.New("Invalid embedding batch response")
	}
	out:=make([]semanticVector,count)
	for _, row:=range result.Data {
		if row.Index==nil || *row.Index<0 || *row.Index>=count || out[*row.Index]!=nil {return nil,errors.New("Invalid embedding batch index")}
		v,err:=normalizeSemanticVector(row.Embedding)
		if err!=nil{return nil,err}
		out[*row.Index]=v
	}
	return out,nil
}

func verifyEmbeddingModel(ctx context.Context, dir string) (string,error) {
	path:=filepath.Join(dir,embeddingResource.name)
	if !modelFileAvailable(path,embeddingResource) {return "",errors.New("Use 'download embedding model' in Local chat first")}
	f,err:=os.Open(path);if err!=nil{return "",err};defer f.Close()
	h:=sha256.New()
	buf:=make([]byte,256<<10)
	for {
		if err=ctx.Err();err!=nil{return "",err}
		n,readErr:=f.Read(buf)
		if n>0{_,_=h.Write(buf[:n])}
		if readErr==io.EOF{break}
		if readErr!=nil{return "",readErr}
	}
	if hex.EncodeToString(h.Sum(nil))!=embeddingResource.sum {return "",errors.New("Embedding model checksum mismatch; remove the invalid model file and download it again")}
	return path,nil
}

func startEmbeddingModel(ctx context.Context, backend, path, logPath string, opts scanSettings) (*localModel,error) {
	args:=[]string{"--ctx-size","512","--batch-size","512","--ubatch-size","512","--parallel","1",
		"--threads",strconv.Itoa(opts.Resources.Threads),"--threads-batch",strconv.Itoa(opts.Resources.BatchThreads),
		"--n-gpu-layers","0","--embedding","--pooling","cls","--no-webui","--no-warmup","--cache-ram","0"}
	return startLocalModelServer(ctx,backend,logPath,opts,path,args)
}

func (m *localModel) embedSemanticBatch(ctx context.Context,texts []string) ([]semanticVector,error) {
	if len(texts)==0 || len(texts)>embeddingBatchSize {return nil,errors.New("Invalid embedding batch size")}
	for _,text:=range texts {if len(text)==0 || len(text)>400{return nil,errors.New("Invalid embedding input size")}}
	body,err:=json.Marshal(map[string]any{"input":texts})
	if err!=nil{return nil,err}
	data,err:=m.request(ctx,"POST","/v1/embeddings",body,time.Duration(m.options().AI.TimeoutSeconds)*time.Second)
	if err!=nil{return nil,fmt.Errorf("Embedding inference: %w",err)}
	return decodeEmbeddingBatch(data,len(texts))
}

func semanticSegments(e evidence) []string {
	var out []string
	seen:=map[string]bool{}
	for _,field:=range []string{e.Title,e.Summary,e.Text,e.FollowupText}{
		text:=chars(strings.Join(strings.Fields(field)," "),400)
		if len(text)<3 || seen[text]{continue};seen[text]=true;out=append(out,text)
	}
	return out
}
