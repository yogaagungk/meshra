//go:build linux

package sandbox

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "io"
 "os"
 "path/filepath"
 "sort"
 "strings"
 "time"
)

var systemRoots=[]string{"/usr","/bin","/lib","/lib64"}
func systemPath(p string)bool{for _,root:=range systemRoots{if p==root||strings.HasPrefix(p,root+"/"){return true}};return false}
func allowedRuntime(p string)bool{
 if !filepath.IsAbs(p)||filepath.Clean(p)!=p||strings.ContainsAny(p,"\x00\n"){return false}
 if systemPath(p){return true};if p=="/etc/ssl/certs"||p=="/etc/fonts"||strings.HasPrefix(p,"/etc/ssl/certs/")||strings.HasPrefix(p,"/etc/fonts/"){return true}
 for _,bad:=range []string{"/","/etc","/run","/var","/proc","/sys","/dev","/tmp","/mnt","/media","/root","/home","/workspace","/home/worker"}{if p==bad||strings.HasPrefix(p,bad+"/")&&bad!="/home"&&bad!="/root"{return false}}
 home,_:=os.UserHomeDir();if p==home||strings.HasPrefix(p,"/home/")&&len(strings.Split(strings.Trim(p,"/"),"/"))<3{return false};for _,part:=range strings.Split(p,"/"){if part==".ssh"||part==".aws"||part==".gnupg"||part==".config"{return false}};return p!=filepath.Join(home,".local")&&p!=filepath.Join(home,".cache")
}
func assetHash(path string)(string,error){
 st,e:=os.Lstat(path);if e!=nil{return "",e};h:=sha256.New()
 if st.Mode().IsRegular(){f,e:=os.Open(path);if e!=nil{return "",e};_,e=io.Copy(h,f);f.Close();return hex.EncodeToString(h.Sum(nil)),e}
 if !st.IsDir(){return "",fmt.Errorf("runtime must be a canonical regular file or directory")}
 root,id,e:=openRoot(path);if e!=nil{return "",e};defer root.Close()
 var walk func(*os.File,string)error
 walk=func(dir *os.File,rel string)error{
  entries,e:=dir.ReadDir(-1);if e!=nil{return e};if len(entries)>100000{return ErrLimit};sort.Slice(entries,func(i,j int)bool{return entries[i].Name()<entries[j].Name()})
  for _,d:=range entries{
   p:=filepath.Join(rel,d.Name())
   if d.Type()&os.ModeSymlink!=0{
    buf:=make([]byte,4097);n,e:=readlink(int(dir.Fd()),d.Name(),buf);if e!=nil{return e};target:=string(buf[:n])
    if filepath.IsAbs(target){
     canonical,e:=filepath.EvalSymlinks(target);if e!=nil||!systemPath(canonical){return fmt.Errorf("external runtime symlink %s",p)}
     st,e:=os.Stat(canonical);if e!=nil||!st.Mode().IsRegular(){return fmt.Errorf("runtime resource symlink must target a system regular file")}
     hash,e:=assetHash(canonical);if e!=nil{return e};fmt.Fprintf(h,"link:%s:%s:%s\x00",p,target,hash);continue
    }
    if strings.HasPrefix(filepath.Clean(filepath.Join(filepath.Dir(p),target)),".."){return fmt.Errorf("external runtime symlink %s",p)}
    fmt.Fprintf(h,"link:%s:%s\x00",p,target);continue
   }
   f,e:=beneath(int(dir.Fd()),d.Name(),0);if e!=nil{return e};mount,e:=mountID(int(f.Fd()));if e!=nil||mount!=id{f.Close();return fmt.Errorf("runtime nested mount")}
   v,e:=entryFor(f);if e!=nil{f.Close();return e};b,_:=json.Marshal(v);fmt.Fprintf(h,"%s\x00%s\x00",p,b);if v.Kind=="directory"{e=walk(f,p)};f.Close();if e!=nil{return e}
  };return nil
 }
 if e=walk(root,"");e!=nil{return "",e};return hex.EncodeToString(h.Sum(nil)),nil
}

// Configure records local files only. It does not install or download anything.
func Configure(c Config)(Config,error){
 c.Limits=c.Limits.defaults();if e:=c.Limits.validate();e!=nil{return c,e};if len(c.Executables)>32||len(c.RuntimePaths)>64{return c,fmt.Errorf("runtime manifest too large")};c.Manifest=nil
 paths:=map[string]bool{}
 for _,p:=range systemRoots{if _,e:=os.Stat(p);e==nil{paths[p]=true}}
 for role,p:=range c.Executables{if role==""||len(role)>64{return c,fmt.Errorf("invalid executable role")};if !filepath.IsAbs(p){return c,fmt.Errorf("executable must be absolute")};canonical,e:=filepath.EvalSymlinks(p);if e!=nil{return c,e};if !allowedRuntime(canonical){return c,ErrDenied};st,e:=os.Stat(canonical);if e!=nil{return c,e};if !st.Mode().IsRegular()||st.Mode().Perm()&0111==0{return c,fmt.Errorf("%s is not an executable",p)};c.Executables[role]=canonical;paths[canonical]=true}
 for i,p:=range c.RuntimePaths{canonical,e:=filepath.EvalSymlinks(p);if e!=nil{return c,e};if !allowedRuntime(canonical){return c,fmt.Errorf("%w: forbidden runtime mount %s",ErrDenied,p)};c.RuntimePaths[i]=canonical;paths[canonical]=true}
 names:=make([]string,0,len(paths));for p:=range paths{names=append(names,p)};sort.Strings(names)
 for _,p:=range names{a:=Asset{Path:p,System:systemPath(p)};if !a.System||!contains(systemRoots,p){hash,e:=assetHash(p);if e!=nil{return c,e};a.SHA256=hash};c.Manifest=append(c.Manifest,a)}
 return c,nil
}
func contains(items []string,v string)bool{for _,x:=range items{if x==v{return true}};return false}
func Validate(c Config)error{
 if e:=c.Limits.defaults().validate();e!=nil{return e};if len(c.Manifest)==0{return fmt.Errorf("%w: runtime.configure must record a manifest",ErrUnavailable)}
 covered:=map[string]bool{};for _,a:=range c.Manifest{if !allowedRuntime(a.Path){return ErrDenied};if a.System!=systemPath(a.Path){return ErrDenied};if e:=rejectNestedMounts(a.Path);e!=nil{return e};covered[a.Path]=true;if a.SHA256!=""{actual,e:=assetHash(a.Path);if e!=nil{return e};if actual!=a.SHA256{return fmt.Errorf("%w: runtime manifest changed: %s",ErrUnavailable,a.Path)}}else if !contains(systemRoots,a.Path){return ErrDenied}}
 for _,p:=range c.Executables{if !covered[p]{return ErrDenied}};for _,p:=range c.RuntimePaths{if !covered[p]{return ErrDenied}};return nil
}
func Check(ctx context.Context,c Config)Status{
 status:=Status{State:"ready",Prerequisites:[]string{}}
 if c.Executables["bwrap"]==""{status.State="missing";status.Prerequisites=append(status.Prerequisites,"Configure an installed bubblewrap executable (executables.bwrap)")}
 if e:=Validate(c);e!=nil{status.State="missing";status.Prerequisites=append(status.Prerequisites,e.Error())}
 cg,e:=newCgroup(c.Limits,Command);if e!=nil{status.State="unsupported";status.Prerequisites=append(status.Prerequisites,e.Error())}else{cg.close()}
 if status.State!="ready"{return status}
 // No model call: exercise the actual mount/namespaces and clone3 cgroup path.
 timeout,cancel:=context.WithTimeout(ctx,5*time.Second);defer cancel()
 dir,e:=os.MkdirTemp("","meshra-runtime-probe-");if e!=nil{status.State="failed";status.Prerequisites=append(status.Prerequisites,e.Error());return status};defer os.RemoveAll(dir)
 snapshot:=&Snapshot{Root:dir};p,e:=prepare(timeout,c,Request{Workspace:snapshot,Argv:[]string{"/usr/bin/true"},Kind:Command,ReadOnly:true},true)
 if e==nil{e=p.Start();if e==nil{e=p.Wait()};p.Close()};if e!=nil{status.State="unsupported";status.Prerequisites=append(status.Prerequisites,"bubblewrap namespaces/clone3 cgroup confinement: "+e.Error())};return status
}
