//go:build linux

package sandbox

import (
 "bytes"
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "os"
 "path/filepath"
 "sort"
 "strings"

 "golang.org/x/sys/unix"
)

// Stage replaces one private working file. Mode zero preserves an existing
// file's permission bits, or selects 0644 for a new file.
func(s *Snapshot)Stage(path string,data []byte,mode os.FileMode)(*Manifest,error){
 if !validPath(path)||len(data)>1<<20||mode&^0777!=0{return nil,fmt.Errorf("invalid bounded regular-file replacement")}
 s.mu.Lock();defer s.mu.Unlock();if s.closed{return nil,ErrDenied}
 storeMu.Lock();defer storeMu.Unlock();if e:=checkStore(s.store,int64(len(data))*2,s.limits);e!=nil{return nil,e}
 root,_,e:=openRoot(s.rootPath);if e!=nil{return nil,e};defer root.Close()
 parent,e:=secureParent(root,path,true);if e!=nil{return nil,e};defer parent.Close();name:=filepath.Base(path)
 // Existing symlinks, directories and devices are never replacement targets.
 var st unix.Stat_t;e=unix.Fstatat(int(parent.Fd()),name,&st,unix.AT_SYMLINK_NOFOLLOW);if e==nil&&st.Mode&unix.S_IFMT!=unix.S_IFREG{return nil,ErrDenied};if e!=nil&&!errors.Is(e,unix.ENOENT){return nil,e}
 if mode==0{mode=0644;if e==nil{mode=os.FileMode(st.Mode&0777)}}
 if e=replaceFile(parent,name,bytes.NewReader(data),mode.Perm());e!=nil{return nil,e}
 return s.changesLocked(context.Background())
}

func(s *Snapshot)Changes(ctx context.Context)(*Manifest,error){s.mu.Lock();defer s.mu.Unlock();storeMu.Lock();defer storeMu.Unlock();if s.closed{return nil,ErrDenied};return s.changesLocked(ctx)}
func(s *Snapshot)changesLocked(ctx context.Context)(m *Manifest,err error){
 root,_,e:=openRoot(s.rootPath);if e!=nil{return nil,e};defer root.Close()
 used,e:=storeSize(s.store);if e!=nil{return nil,e};used+=4096;if used>s.limits.StoreBytes{return nil,ErrLimit}
 dir,e:=os.MkdirTemp(s.store,"manifest-");if e!=nil{return nil,e};defer func(){if err!=nil{os.RemoveAll(dir)}}()
 m=&Manifest{directory:dir,store:s.store,limits:s.limits,Changes:[]Change{}};current:=map[string]Entry{};var size int64;count:=0
 var walk func(*os.File,string)error
 walk=func(src *os.File,rel string)error{for{entries,e:=src.ReadDir(128);if e!=nil&&e!=io.EOF{return e};for _,d:=range entries{
  if e=contextError(ctx);e!=nil{return e};p:=filepath.Join(rel,d.Name());if !validPath(p){return fmt.Errorf("%w: protected output path",ErrDenied)};count++;if count>s.limits.SnapshotFiles{return ErrLimit}
  if d.Type()&os.ModeSymlink!=0 {buf:=make([]byte,4097);n,e:=unix.Readlinkat(int(src.Fd()),d.Name(),buf);if e!=nil{return e};v:=Entry{Kind:"symlink",Link:string(buf[:n]),Mode:0777};if old,ok:=s.original[p];!ok||old!=v{return fmt.Errorf("%w: output symlink: %s",ErrDenied,p)};current[p]=v;continue}
  f,e:=beneath(int(src.Fd()),d.Name(),unix.O_RDONLY|unix.O_NONBLOCK);if e!=nil{return e};st,e:=f.Stat();if e!=nil{f.Close();return e};if st.Mode().IsRegular(){size+=st.Size();if size>s.limits.SnapshotBytes{f.Close();return ErrLimit}};v,e:=entryForLimited(f,st.Size());if e!=nil{f.Close();return e};current[p]=v
  if v.Kind=="directory"{e=walk(f,p);f.Close();if e!=nil{return e};continue}
  before,ok:=s.original[p];if ok&&before==v{f.Close();continue}
  blob:=filepath.Join(dir,v.Hash);if _,e=os.Stat(blob);e==nil{f.Close();continue}else if !os.IsNotExist(e){f.Close();return e};used+=st.Size()+4096;if used>s.limits.StoreBytes{f.Close();return ErrLimit}
  out,e:=os.OpenFile(blob,os.O_WRONLY|os.O_CREATE|os.O_EXCL,0400);if e!=nil{f.Close();return e};n,e:=io.Copy(out,io.LimitReader(f,st.Size()+1));f.Close();if e==nil&&n!=st.Size(){e=ErrConflict};if e==nil{e=out.Sync()};out.Close();if e!=nil{return e};check,e:=os.Open(blob);if e!=nil{return e};actual,e:=entryFor(check);check.Close();if e!=nil{return e};if actual.Hash!=v.Hash{return ErrConflict}
 };if e==io.EOF{break}};return nil}
 if err=walk(root,"");err!=nil{return nil,err}
 keys:=map[string]bool{};for p:=range current{keys[p]=true};for p:=range s.original{keys[p]=true};paths:=make([]string,0,len(keys));for p:=range keys{paths=append(paths,p)};sort.Strings(paths)
 for _,p:=range paths{old,had:=s.original[p];next,has:=current[p];if had&&has&&old==next{continue};if had&&old.Kind=="symlink"{return nil,fmt.Errorf("%w: changing original symlink",ErrDenied)};if had&&has&&old.Kind!=next.Kind{return nil,fmt.Errorf("%w: file/directory type changes need separate changesets",ErrDenied)}
  c:=Change{Path:p};if had{x:=old;c.Before=&x};if has{x:=next;c.After=&x;if next.Kind=="file"{c.Blob=filepath.Join(dir,next.Hash)}};m.Changes=append(m.Changes,c)
 }
 m.renderPreview(s.originalText);m.seal();return m,nil
}

func(m *Manifest)Close()error{if m.directory==""{return nil};return os.RemoveAll(m.directory)}
func(m *Manifest)Save()error{
 storeMu.Lock();defer storeMu.Unlock()
 if e:=m.validate();e!=nil{return e}
 for _,c:=range m.Changes{if c.After!=nil&&c.After.Kind=="file"{f,e:=os.Open(c.Blob);if e!=nil{return e};v,e:=entryFor(f);f.Close();if e!=nil{return e};if v.Hash!=c.After.Hash{return ErrConflict}}}
 b,e:=json.Marshal(m);if e!=nil{return e};if len(b)>32<<20{return ErrLimit};if m.store!=""{if e=checkStore(m.store,int64(len(b))+4096,m.limits);e!=nil{return e}};f,e:=os.OpenFile(filepath.Join(m.directory,"manifest.json"),os.O_CREATE|os.O_EXCL|os.O_WRONLY,0400);if e!=nil{return e};_,e=f.Write(b);if e==nil{e=f.Sync()};f.Close();if e!=nil{return e};dir,e:=os.Open(m.directory);if e!=nil{return e};e=dir.Sync();dir.Close();return e
}
func(m *Manifest)Directory()string{return m.directory}
func LoadManifest(dir,hash string)(*Manifest,error){f,e:=os.Open(filepath.Join(dir,"manifest.json"));if e!=nil{return nil,e};b,e:=io.ReadAll(io.LimitReader(f,32<<20+1));f.Close();if e!=nil{return nil,e};if len(b)>32<<20{return nil,ErrLimit};var m Manifest;if e=json.Unmarshal(b,&m);e!=nil{return nil,e};if e=m.validate();e!=nil{return nil,e};m.directory=dir;expected:=m.Hash;m.seal();if m.Hash!=expected||m.Hash!=hash{return nil,ErrConflict};for i:=range m.Changes{c:=&m.Changes[i];if c.After!=nil&&c.After.Kind=="file"{c.Blob=filepath.Join(dir,c.After.Hash)}};return &m,nil}

func mkdirAt(parent *os.File,name string,mode uint32)error{
 if e:=unix.Mkdirat(int(parent.Fd()),name,mode|0700);e!=nil{return e}
 fd,e:=unix.Openat(int(parent.Fd()),name,unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,0)
 if e!=nil{return e}
 defer unix.Close(fd)
 // O_PATH permits exact mode restoration even with a restrictive daemon umask.
 // AT_EMPTY_PATH changes only this freshly created directory, never a file
 // hardlink substituted at the directory entry.
 if e=unix.Fchmodat(fd,"",mode,unix.AT_EMPTY_PATH);e!=nil{
  return fmt.Errorf("%w: descriptor-based fchmodat2 directory modes: %v",ErrUnavailable,e)
 }
 return nil
}
func secureParent(root *os.File,path string,create bool)(*os.File,error){
 parent,e:=beneath(int(root.Fd()),".",unix.O_RDONLY|unix.O_DIRECTORY)
 if e!=nil{return nil,e}
 if filepath.Dir(path)=="."{return parent,nil}
 for _,part:=range strings.Split(filepath.Dir(path),"/"){
  next,e:=beneath(int(parent.Fd()),part,unix.O_RDONLY|unix.O_DIRECTORY)
  if errors.Is(e,unix.ENOENT)&&create{
   e=mkdirAt(parent,part,0755)
   if e==nil{e=unix.Fsync(int(parent.Fd()))}
   if e==nil{next,e=beneath(int(parent.Fd()),part,unix.O_RDONLY|unix.O_DIRECTORY)}
  }
  parent.Close()
  if e!=nil{return nil,e}
  parent=next
 }
 return parent,nil
}
func replaceFile(parent *os.File,name string,src io.Reader,mode os.FileMode)(err error){
 temp,e:=temporaryName();if e!=nil{return e}
 fd,e:=unix.Openat(int(parent.Fd()),temp,unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,0600);if e!=nil{return e};out:=os.NewFile(uintptr(fd),temp);defer func(){out.Close();unix.Unlinkat(int(parent.Fd()),temp,0)}()
 if _,err=io.Copy(out,src);err!=nil{return err};if err=out.Chmod(mode.Perm());err!=nil{return err};if err=out.Sync();err!=nil{return err};if err=out.Close();err!=nil{return err};if err=unix.Renameat(int(parent.Fd()),temp,int(parent.Fd()),name);err!=nil{return err};return unix.Fsync(int(parent.Fd()))
}
func currentEntry(root *os.File,path string)(*Entry,error){f,e:=beneath(int(root.Fd()),path,unix.O_RDONLY|unix.O_NONBLOCK);if errors.Is(e,unix.ENOENT){return nil,nil};if e!=nil{return nil,e};defer f.Close();v,e:=entryForLimited(f,4<<30);if errors.Is(e,ErrLimit){return nil,ErrConflict};return &v,e}
func sameEntry(a,b *Entry)bool{if a==nil||b==nil{return a==b};return *a==*b}

// Apply never retries. A journal failure after rename means the persisted applying
// entry is uncertain; callers must report it and must not replay the changeset.
func(m *Manifest)Apply(ctx context.Context,workspace string,claim Claim,journal Journal)error{
 if claim==nil||journal==nil{return ErrDenied};expected:=m.Hash;m.seal();if m.Hash!=expected{m.Hash=expected;return ErrConflict}
 if e:=m.validate();e!=nil{return e}
 root,_,e:=openRoot(workspace);if e!=nil{return e};defer root.Close()
 for _,c:=range m.Changes{if !validPath(c.Path){return ErrDenied};v,e:=currentEntry(root,c.Path);if e!=nil{return e};if !sameEntry(v,c.Before){return fmt.Errorf("%w: %s",ErrConflict,c.Path)};if c.After!=nil&&c.After.Kind=="file"{f,e:=os.Open(c.Blob);if e!=nil{return e};actual,e:=entryFor(f);f.Close();if e!=nil{return e};if actual.Hash!=c.After.Hash{return ErrConflict}}}
 if e=claim();e!=nil{return e}
 changes:=append([]Change(nil),m.Changes...)
 sort.Slice(changes,func(i,j int)bool{
  a,b:=changes[i],changes[j]
  category:=func(c Change)int{if c.After!=nil{if c.After.Kind=="directory"{return 0};return 1};if c.Before.Kind=="file"{return 2};return 3}
  ac,bc:=category(a),category(b)
  if ac!=bc{return ac<bc}
  if ac==3{ad,bd:=strings.Count(a.Path,"/"),strings.Count(b.Path,"/");if ad!=bd{return ad>bd}}
  return a.Path<b.Path
 })
 for _,c:=range changes{
  if e=contextError(ctx);e!=nil{return e};if e=claim();e!=nil{return e};parent,e:=secureParent(root,c.Path,false);if e!=nil{return e}
  v,e:=currentEntry(root,c.Path);if e==nil&&!sameEntry(v,c.Before){e=fmt.Errorf("%w: %s",ErrConflict,c.Path)};if e!=nil{parent.Close();return e}
  record:=ApplyRecord{Path:c.Path,State:"applying"};if c.Before!=nil{record.OriginalHash=c.Before.Hash};if c.After!=nil{record.ResultHash=c.After.Hash};if e=journal(record);e!=nil{parent.Close();return e};if e=claim();e!=nil{parent.Close();return e}
  name:=filepath.Base(c.Path)
  if c.After==nil{flags:=0;if c.Before.Kind=="directory"{flags=unix.AT_REMOVEDIR};e=unix.Unlinkat(int(parent.Fd()),name,flags)}else if c.After.Kind=="directory"{if c.Before==nil{e=mkdirAt(parent,name,c.After.Mode|0700)}}else if c.After.Kind=="file"{f,err:=os.Open(c.Blob);e=err;if e==nil{e=replaceFile(parent,name,f,os.FileMode(c.After.Mode));f.Close()}}else{e=ErrDenied}
  if e==nil{e=unix.Fsync(int(parent.Fd()))};parent.Close();if e!=nil{record.State="uncertain";record.Error=e.Error();_ = journal(record);return e};record.State="applied";if c.After!=nil&&c.After.Kind=="directory"{record.State="pending_mode";if c.Before==nil{record.State="created"}};if e=journal(record);e!=nil{return e}
 }
 // Restore final directory modes only after children are published. This also
 // supports new read-only directories without weakening the original workspace.
 for i:=len(changes)-1;i>=0;i--{
  c:=changes[i]
  if c.After==nil||c.After.Kind!="directory"{continue}
  if e=claim();e!=nil{return e}
  f,e:=beneath(int(root.Fd()),c.Path,unix.O_RDONLY|unix.O_DIRECTORY)
  if e!=nil{return e}
  record:=ApplyRecord{Path:c.Path,State:"mode_uncertain"}
  if c.Before==nil{record.State="created_mode_uncertain"}
  if e=journal(record);e!=nil{f.Close();return e}
  if e=claim();e!=nil{f.Close();return e}
  e=f.Chmod(os.FileMode(c.After.Mode))
  if e==nil{e=f.Sync()}
  f.Close()
  if e!=nil{record.Error=e.Error();_ = journal(record);return e}
  record.State="applied"
  if e=journal(record);e!=nil{return e}
 }
 return nil
}
