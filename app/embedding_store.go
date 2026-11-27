package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

func semanticVectorDir(runDir string) string {return filepath.Join(runDir,"semantic-vectors")}

func semanticInputKey(text string) string {
	h:=sha256.Sum256([]byte(embeddingProtocol+"\x00"+embeddingResource.sum+"\x00"+text))
	return hex.EncodeToString(h[:])
}

type storedSemanticVector struct {
	Protocol, Model, Key string
	Vector semanticVector
}

func readSemanticVector(dir,text string) (semanticVector,bool) {
	key:=semanticInputKey(text)
	path:=filepath.Join(dir,key+".json")
	info,err:=os.Lstat(path)
	if err!=nil || !info.Mode().IsRegular() || linked(info) || info.Size()>64<<10 {return nil,false}
	f,err:=os.Open(path);if err!=nil{return nil,false};defer f.Close()
	var rec storedSemanticVector
	if json.NewDecoder(io.LimitReader(f,64<<10)).Decode(&rec)!=nil || rec.Key!=key || rec.Model!=embeddingResource.sum || rec.Protocol!=embeddingProtocol{return nil,false}
	v,err:=normalizeSemanticVector(rec.Vector)
	return v,err==nil
}

func saveSemanticVector(dir,text string,v semanticVector) error {
	v,err:=normalizeSemanticVector(v);if err!=nil{return err}
	key:=semanticInputKey(text)
	return atomicJSON(filepath.Join(dir,key+".json"),storedSemanticVector{embeddingProtocol,embeddingResource.sum,key,v})
}

func clearSemanticVectors(runDir string) error {
	return walkDigestJSONFiles(semanticVectorDir(runDir),func(path string,_ os.FileInfo)error{return os.Remove(path)})
}

func requireSemanticVectorDir(runDir string) (string,error) {
	dir:=semanticVectorDir(runDir)
	if err:=checkLocalDataDirectory(dir,true);err!=nil{return "",err}
	return dir,nil
}
