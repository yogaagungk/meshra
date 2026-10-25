//go:build linux

package sandbox

import (
 "encoding/hex"
 "fmt"
 "os"
 "path/filepath"
 "strings"
)

func validHash(value string)bool{if len(value)!=64{return false};b,e:=hex.DecodeString(value);return e==nil&&len(b)==32}
func(m *Manifest)validate()error{
 if len(m.Changes)>100000{return ErrLimit};seen:=map[string]bool{}
 for _,c:=range m.Changes{if !validPath(c.Path)||seen[c.Path]||c.Before==nil&&c.After==nil{return ErrDenied};seen[c.Path]=true
  for _,v:=range []*Entry{c.Before,c.After}{if v==nil{continue};if v.Mode&^0777!=0||v.Link!=""{return ErrDenied};switch v.Kind{case "file":if !validHash(v.Hash){return ErrDenied};case "directory":if v.Hash!=""{return ErrDenied};default:return ErrDenied}}
  if c.Before!=nil&&c.After!=nil&&c.Before.Kind!=c.After.Kind{return ErrDenied}
 };return nil
}
func rejectNestedMounts(path string)error{
 canonical,e:=filepath.EvalSymlinks(path);if e!=nil{return e}
 st,e:=os.Stat(canonical);if e!=nil{return e}
 b,e:=os.ReadFile("/proc/self/mountinfo")
 if e!=nil{return fmt.Errorf("%w: mountinfo required",ErrUnavailable)}
 unescape:=strings.NewReplacer("\\040"," ","\\011","\t","\\012","\n","\\134","\\")
 filesystem:="";matched:=-1
 for _,line:=range strings.Split(string(b),"\n"){
  fields:=strings.Fields(line)
  if len(fields)<6{continue}
  mount:=unescape.Replace(fields[4])
  if st.IsDir()&&strings.HasPrefix(mount,canonical+"/"){return fmt.Errorf("%w: runtime path contains nested host mount %s; choose an installation without service/Windows mounts",ErrUnavailable,mount)}
  if mount=="/"||canonical==mount||strings.HasPrefix(canonical,mount+"/"){
   if len(mount)>matched{
    for i,value:=range fields{if value=="-"&&i+1<len(fields){filesystem=fields[i+1];matched=len(mount);break}}
   }
  }
 }
 switch filesystem{
 case "9p","drvfs","cifs","smb3":
  return fmt.Errorf("%w: Windows/shared-drive runtime mounts (%s) are not permitted",ErrUnavailable,filesystem)
 case "":
  return fmt.Errorf("%w: runtime mount could not be identified",ErrUnavailable)
 }
 return nil
}
