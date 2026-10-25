//go:build linux

package sandbox

import (
 "os"
 "path/filepath"
)

// canonicalDestination resolves existing ancestor links before creating a store,
// so an alias cannot cause snapshot materialization inside the original root.
func canonicalDestination(path string)(string,error){
 absolute,e:=filepath.Abs(path)
 if e!=nil{return "",e}
 current:=absolute
 missing:=[]string{}
 for {
  _,e=os.Lstat(current)
  if e==nil{break}
  if !os.IsNotExist(e){return "",e}
  parent:=filepath.Dir(current)
  if parent==current{return "",e}
  missing=append(missing,filepath.Base(current))
  current=parent
 }
 current,e=filepath.EvalSymlinks(current)
 if e!=nil{return "",e}
 for i:=len(missing)-1;i>=0;i--{current=filepath.Join(current,missing[i])}
 return current,nil
}
