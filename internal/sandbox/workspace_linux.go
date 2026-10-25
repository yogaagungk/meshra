//go:build linux

package sandbox

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "fmt"
 "io"
 "os"
 "path/filepath"
 "strings"
 "sync"

 "golang.org/x/sys/unix"
)

var storeMu sync.Mutex

type Snapshot struct {
 Root string
 rootPath string
 original map[string]Entry
 originalText map[string]string
 store string
 limits Limits
 mu sync.Mutex
 closed bool
 workerRefs int
}

func openRoot(path string) (*os.File,uint64,error) {
 fd,e:=unix.Open(path,unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,0); if e!=nil{return nil,0,e}
 f:=os.NewFile(uintptr(fd),path); id,e:=mountID(fd); if e!=nil {f.Close();return nil,0,e};return f,id,nil
}
func mountID(fd int)(uint64,error){var st unix.Statx_t; e:=unix.Statx(fd,"",unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW,unix.STATX_MNT_ID,&st);if e!=nil{return 0,e};if st.Mask&unix.STATX_MNT_ID==0{return 0,fmt.Errorf("%w: statx mount IDs required",ErrUnavailable)};return st.Mnt_id,nil}
func beneath(fd int,path string,flags int)(*os.File,error){n,e:=unix.Openat2(fd,path,&unix.OpenHow{Flags:uint64(flags|unix.O_CLOEXEC|unix.O_NOFOLLOW),Resolve:unix.RESOLVE_BENEATH|unix.RESOLVE_NO_MAGICLINKS|unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_XDEV});if e==unix.ELOOP||e==unix.EXDEV{return nil,fmt.Errorf("%w: symlink or nested mount traversal",ErrDenied)};if e==unix.ENOSYS||e==unix.EINVAL{return nil,fmt.Errorf("%w: openat2 rooted no-follow resolution required",ErrUnavailable)};if e!=nil{return nil,e};return os.NewFile(uintptr(n),path),nil}
func entryFor(f *os.File)(Entry,error){return entryForLimited(f,1<<63-1)}
func entryForLimited(f *os.File,limit int64)(Entry,error){
 st,e:=f.Stat()
 if e!=nil{return Entry{},e}
 v:=Entry{Mode:uint32(st.Mode().Perm())}
 if st.IsDir(){v.Kind="directory";return v,nil}
 if !st.Mode().IsRegular(){return v,fmt.Errorf("%w: non-regular workspace entry",ErrDenied)}
 if st.Size()>limit{return v,ErrLimit}
 v.Kind="file"
 h:=sha256.New()
 n,e:=io.Copy(h,io.LimitReader(f,st.Size()+1))
 if e!=nil{return v,e}
 if n!=st.Size(){return v,ErrConflict}
 v.Hash=hex.EncodeToString(h.Sum(nil))
 _,e=f.Seek(0,0)
 return v,e
}
func storeSize(path string)(int64,error){var size int64;e:=filepath.WalkDir(path,func(p string,d os.DirEntry,e error)error{if e!=nil{return e};size+=4096;if d.Type().IsRegular(){st,e:=d.Info();if e!=nil{return e};size+=st.Size()};return nil});if missing(e){return 0,nil};return size,e}
func checkStore(path string,extra int64,l Limits)error{size,e:=storeSize(path);if e!=nil{return e};if size+extra>l.StoreBytes{return fmt.Errorf("%w: aggregate sandbox store quota",ErrLimit)};return nil}

func SnapshotWorkspace(ctx context.Context,workspace,store string,limits Limits)(s *Snapshot,err error){
 limits=limits.defaults();if err=limits.validate();err!=nil{return nil,err}
 workspace,err=filepath.Abs(workspace);if err!=nil{return nil,err};workspace,err=filepath.EvalSymlinks(workspace);if err!=nil{return nil,err};store,err=canonicalDestination(store);if err!=nil{return nil,err};storeRel,e:=filepath.Rel(workspace,store);if e!=nil{return nil,e};if storeRel=="."||storeRel!=".."&&!strings.HasPrefix(storeRel,".."+string(os.PathSeparator)){return nil,fmt.Errorf("%w: sandbox store cannot be inside selected workspace",ErrDenied)}
 storeMu.Lock();defer storeMu.Unlock()
 if err=os.MkdirAll(store,0700);err!=nil{return nil,err};if err=os.Chmod(store,0700);err!=nil{return nil,err};used,err:=storeSize(store);if err!=nil{return nil,err};used+=4096;if used>limits.StoreBytes{return nil,ErrLimit}
 root,id,e:=openRoot(workspace);if e!=nil{return nil,e};defer root.Close()
 dir,e:=os.MkdirTemp(store,"snapshot-");if e!=nil{return nil,e};defer func(){if err!=nil{removePrivate(dir)}}();if err=os.Chmod(dir,0700);err!=nil{return nil,err}
 s=&Snapshot{Root:dir,rootPath:dir,store:store,limits:limits,original:map[string]Entry{},originalText:map[string]string{}}
 var bytes int64;var count int;previewBytes:=int64(0)
 var walk func(*os.File,string)error
 walk=func(src *os.File,rel string)error{
  for { entries,e:=src.ReadDir(128);if e!=nil&&e!=io.EOF{return e}
  for _,d:=range entries{
   if e=contextError(ctx);e!=nil{return e};if protectedName(d.Name()){continue};p:=filepath.Join(rel,d.Name());if !validPath(p){return fmt.Errorf("%w: invalid workspace path",ErrDenied)}
   count++;if count>limits.SnapshotFiles{return fmt.Errorf("%w: snapshot entry count",ErrLimit)};if used+bytes+int64(count)*4096>limits.StoreBytes{return ErrLimit}
   var st unix.Statx_t;if e=unix.Statx(int(src.Fd()),d.Name(),unix.AT_SYMLINK_NOFOLLOW,unix.STATX_MNT_ID|unix.STATX_TYPE,&st);e!=nil{return e};if st.Mnt_id!=id{return fmt.Errorf("%w: nested workspace mount: %s",ErrDenied,p)}
   dst:=filepath.Join(dir,p)
   if d.Type()&os.ModeSymlink!=0 {
    buf:=make([]byte,4097);n,e:=unix.Readlinkat(int(src.Fd()),d.Name(),buf);if e!=nil{return e};link:=string(buf[:n]);if n>4096||strings.ContainsAny(link,"\\\x00"){return fmt.Errorf("%w: invalid symlink: %s",ErrDenied,p)}
    target,e:=filepath.EvalSymlinks(filepath.Join(workspace,p));if e!=nil{return fmt.Errorf("%w: unresolved symlink: %s",ErrDenied,p)};resolved,e:=filepath.Rel(workspace,target);if e!=nil||resolved!="."&&!validPath(resolved){return fmt.Errorf("%w: external/protected symlink: %s",ErrDenied,p)}
    // Absolute internal links are rebased to a relative target, so the copied
    // link never names the original workspace. Resolve final targets no-follow.
    fd,e:=unix.Openat2(int(root.Fd()),resolved,&unix.OpenHow{Flags:unix.O_PATH|unix.O_CLOEXEC,Resolve:unix.RESOLVE_BENEATH|unix.RESOLVE_NO_MAGICLINKS|unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_XDEV});if e!=nil{return fmt.Errorf("%w: symlink target: %s",ErrDenied,p)};unix.Close(fd)
    if filepath.IsAbs(link){link,e=filepath.Rel(filepath.Dir(p),resolved);if e!=nil{return e}}
    if e=os.Symlink(link,dst);e!=nil{return e};s.original[p]=Entry{Kind:"symlink",Link:link,Mode:0777};continue
   }
   f,e:=beneath(int(src.Fd()),d.Name(),unix.O_RDONLY|unix.O_NONBLOCK);if e!=nil{return e};stat,e:=f.Stat();if e!=nil{f.Close();return e};if stat.Mode().IsRegular(){bytes+=stat.Size();if bytes>limits.SnapshotBytes||used+bytes+int64(count)*4096>limits.StoreBytes{f.Close();return ErrLimit}};v,e:=entryForLimited(f,stat.Size());if e!=nil{f.Close();return e}
   s.original[p]=v
   if v.Kind=="directory"{if e=os.Mkdir(dst,0700);e==nil{e=os.Chmod(dst,0700)};if e==nil{e=walk(f,p)};f.Close();if e!=nil{return e};if e=os.Chmod(dst,os.FileMode(v.Mode));e!=nil{return e};continue}
   if stat.Size()<=64<<10-previewBytes{b,e:=io.ReadAll(io.LimitReader(f,stat.Size()));if e!=nil{f.Close();return e};if _,e=f.Seek(0,0);e!=nil{f.Close();return e};s.originalText[p]=string(b);previewBytes+=int64(len(b))}
   out,e:=os.OpenFile(dst,os.O_CREATE|os.O_EXCL|os.O_WRONLY,0600);if e!=nil{f.Close();return e};n,e:=io.Copy(out,io.LimitReader(f,stat.Size()+1));f.Close();if e==nil&&n!=stat.Size(){e=ErrConflict};if e==nil{e=out.Chmod(os.FileMode(v.Mode))};closeErr:=out.Close();if e==nil{e=closeErr};if e!=nil{return e}
   // Hash the copied bytes, not an earlier read of a concurrently changed inode.
   copied,e:=os.Open(dst);if e!=nil{return e};vCopy,e:=entryFor(copied);copied.Close();if e!=nil{return e};if vCopy.Hash!=v.Hash{return ErrConflict}
  };if e==io.EOF{break} };return nil
 }
 if err=walk(root,"");err!=nil{return nil,err};return s,nil
}
func removePrivate(path string)error{
 info,e:=os.Lstat(path)
 if os.IsNotExist(e){return nil}
 if e!=nil{return e}
 if !info.IsDir(){return os.Remove(path)}
 fd,e:=unix.Open(path,unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,0)
 if e!=nil{return e}
 defer unix.Close(fd)
 var clear func(int)error
 clear=func(dirfd int)error{
  if e:=unix.Fchmodat(dirfd,"",0700,unix.AT_EMPTY_PATH);e!=nil{return e}
  dir,e:=beneath(dirfd,".",unix.O_RDONLY|unix.O_DIRECTORY)
  if e!=nil{return e}
  defer dir.Close()
  for{
   entries,e:=dir.ReadDir(128)
   if e!=nil&&e!=io.EOF{return e}
   for _,entry:=range entries{
    child,openErr:=unix.Openat2(dirfd,entry.Name(),&unix.OpenHow{Flags:unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,Resolve:unix.RESOLVE_BENEATH|unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_MAGICLINKS|unix.RESOLVE_NO_XDEV})
    if openErr==nil{
     clearErr:=clear(child)
     unix.Close(child)
     if clearErr!=nil{return clearErr}
     if e:=unix.Unlinkat(dirfd,entry.Name(),unix.AT_REMOVEDIR);e!=nil{return e}
    }else if openErr==unix.ENOTDIR||openErr==unix.ELOOP{
     if e:=unix.Unlinkat(dirfd,entry.Name(),0);e!=nil&&e!=unix.ENOENT{return e}
    }else if openErr!=unix.ENOENT{return openErr}
   }
   if e==io.EOF{break}
  }
  return nil
 }
 if e=clear(fd);e!=nil{return e}
 return os.Remove(path)
}
func(s *Snapshot)acquireWorker()error{s.mu.Lock();defer s.mu.Unlock();if s.closed{return ErrDenied};s.workerRefs++;return nil}
func(s *Snapshot)releaseWorker(){s.mu.Lock();defer s.mu.Unlock();s.workerRefs--}
func(s *Snapshot)Close()error{s.mu.Lock();defer s.mu.Unlock();if s.closed{return nil};if s.workerRefs!=0{return fmt.Errorf("%w: private snapshot is still held by a prepared worker",ErrConflict)};if e:=removePrivate(s.rootPath);e!=nil{return e};s.closed=true;return nil}

// ReadWorkspace is for a trusted caller that has already checked workspaceRead.
func ReadWorkspace(workspace,path string,offset int64,limit int)([]byte,error){
 if !validPath(path)||offset<0||limit<1||limit>64<<10{return nil,fmt.Errorf("invalid bounded read")}
 root,_,e:=openRoot(workspace);if e!=nil{return nil,e};defer root.Close();f,e:=beneath(int(root.Fd()),path,unix.O_RDONLY|unix.O_NONBLOCK);if e!=nil{return nil,e};defer f.Close();st,e:=f.Stat();if e!=nil{return nil,e};if !st.Mode().IsRegular(){return nil,ErrDenied};if _,e=f.Seek(offset,0);e!=nil{return nil,e};return io.ReadAll(io.LimitReader(f,int64(limit)))
}
func(s *Snapshot)Read(path string,offset int64,limit int,allowed bool)([]byte,error){if !allowed{return nil,ErrDenied};return ReadWorkspace(s.rootPath,path,offset,limit)}

// OpenRegular retains a stable descriptor for approved publication. The caller
// must compare the streamed digest before committing immutable attachment bytes.
func(s *Snapshot)OpenRegular(path string)(*os.File,Entry,error){
 s.mu.Lock();defer s.mu.Unlock();if s.closed||!validPath(path){return nil,Entry{},ErrDenied}
 root,_,e:=openRoot(s.rootPath);if e!=nil{return nil,Entry{},e};defer root.Close()
 f,e:=beneath(int(root.Fd()),path,unix.O_RDONLY|unix.O_NONBLOCK);if e!=nil{return nil,Entry{},e}
 info,e:=f.Stat();if e!=nil{f.Close();return nil,Entry{},e};if !info.Mode().IsRegular(){f.Close();return nil,Entry{},ErrDenied};if info.Size()>25<<20{f.Close();return nil,Entry{},ErrLimit}
 entry,e:=entryForLimited(f,25<<20);if e!=nil{f.Close();return nil,Entry{},e};return f,entry,nil
}
