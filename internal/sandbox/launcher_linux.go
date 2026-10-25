//go:build linux

package sandbox

import (
 "bytes"
 "context"
 "errors"
 "fmt"
 "io"
 "os"
 "os/exec"
 "path/filepath"
 "slices"
 "strconv"
 "strings"
 "sync"
 "syscall"
 "time"

 "golang.org/x/sys/unix"
)

var workers=make(chan struct{},4)
func readlink(fd int,name string,b []byte)(int,error){return unix.Readlinkat(fd,name,b)}

func ActiveWorkers()int{return len(workers)}
type cgroup struct { dir string; fd *os.File }
func newCgroup(l Limits,kind Kind)(*cgroup,error){
 root:=l.CgroupRoot
 if root==""{b,e:=os.ReadFile("/proc/self/cgroup");if e!=nil{return nil,e};for _,line:=range strings.Split(string(b),"\n"){if strings.HasPrefix(line,"0::"){rel:=strings.TrimPrefix(line,"0::");if strings.Contains(rel,".."){return nil,fmt.Errorf("%w: configure a delegated cgroupRoot",ErrUnavailable)};root=filepath.Join("/sys/fs/cgroup",rel);break}}}
 if root==""||!filepath.IsAbs(root){return nil,fmt.Errorf("%w: delegated cgroup-v2 root required",ErrUnavailable)}
 var fs unix.Statfs_t;if e:=unix.Statfs(root,&fs);e!=nil||fs.Type!=unix.CGROUP2_SUPER_MAGIC{return nil,fmt.Errorf("%w: cgroupRoot must be delegated cgroup v2",ErrUnavailable)}
 controls,e:=os.ReadFile(filepath.Join(root,"cgroup.subtree_control"));if e!=nil||!strings.Contains(" "+string(controls)+" ","memory")||!strings.Contains(" "+string(controls)+" ","pids"){return nil,fmt.Errorf("%w: operator must delegate and enable cgroup-v2 memory and pids controllers at %s",ErrUnavailable,root)}
 dir,e:=os.MkdirTemp(root,"meshra-worker-");if e!=nil{return nil,fmt.Errorf("%w: writable delegated cgroup required: %v",ErrUnavailable,e)}
 cg:=&cgroup{dir:dir};defer func(){if cg.fd==nil{os.Remove(dir)}}()
 memory:=int64(1<<30);pids:=128;if kind==Browser||kind==Voice{memory=2<<30;pids=256}
 for file,value:=range map[string]string{"memory.max":strconv.FormatInt(memory,10),"memory.swap.max":"0","memory.oom.group":"1","pids.max":strconv.Itoa(pids)}{if e=os.WriteFile(filepath.Join(dir,file),[]byte(value),0600);e!=nil{return nil,fmt.Errorf("%w: enforce %s: %v",ErrUnavailable,file,e)}}
 if _,e=os.Stat(filepath.Join(dir,"cgroup.kill"));e!=nil{return nil,fmt.Errorf("%w: cgroup.kill required",ErrUnavailable)}
 if e=cg.kill();e!=nil{return nil,fmt.Errorf("%w: writable cgroup.kill required: %v",ErrUnavailable,e)}
 cg.fd,e=os.Open(dir);if e!=nil{return nil,e};return cg,nil
}
func(c *cgroup)kill()error{if c==nil{return nil};return os.WriteFile(filepath.Join(c.dir,"cgroup.kill"),[]byte("1"),0600)}
func(c *cgroup)close()error{
 if c==nil{return nil}
 killErr:=c.kill()
 if c.fd!=nil{c.fd.Close()}
 var cleanupErr error
 for range 100{
  cleanupErr=os.Remove(c.dir)
  if cleanupErr==nil||errors.Is(cleanupErr,os.ErrNotExist){return killErr}
  time.Sleep(10*time.Millisecond)
 }
 return errors.Join(killErr,fmt.Errorf("confined cgroup cleanup failed: %w",cleanupErr))
}

type Prepared struct {
 Cmd *exec.Cmd
 cg *cgroup
 ctx context.Context
 cancel context.CancelFunc
 done chan struct{}
 mu sync.Mutex
 started bool
 waitOnce sync.Once
 closeOnce sync.Once
 closeDone chan struct{}
 closed bool
 waitErr error
 closeErr error
 reserved bool
 cleanup []string
 beforeStart func() error
 config Config
 workspace *Snapshot
 executable string
 sealedArgs []string
}
func Prepare(ctx context.Context,c Config,q Request)(*Prepared,error){return prepare(ctx,c,q,false)}
func prepare(ctx context.Context,c Config,q Request,probe bool)(p *Prepared,err error){
 if err=Validate(c);err!=nil{return nil,err};if c.Executables["bwrap"]==""{return nil,fmt.Errorf("%w: bubblewrap executable is not configured",ErrUnavailable)}
 if q.Kind==""{q.Kind=Command};if q.Kind!=Command&&q.Kind!=Browser&&q.Kind!=Voice&&q.Kind!=ACP{return nil,fmt.Errorf("invalid worker kind")}
 if q.Workspace==nil||q.Workspace.Root==""{return nil,fmt.Errorf("private workspace snapshot is required")}
 if !probe&&(q.Workspace.rootPath==""||q.Workspace.Root!=q.Workspace.rootPath||q.Workspace.original==nil){return nil,ErrDenied}
 if q.Kind==ACP&&!q.ReadOnly{return nil,fmt.Errorf("%w: ACP workspace must be read-only",ErrDenied)}
 if (q.Kind==Command||q.Kind==Voice)&&(len(q.AuthFiles)!=0||q.State!=nil){return nil,ErrDenied}
 if (q.Kind==Command||q.Kind==Voice)&&len(q.Env)!=0{return nil,ErrDenied}
 if len(q.Argv)==0||len(q.Argv)>256||!filepath.IsAbs(q.Argv[0]){return nil,fmt.Errorf("explicit absolute argv executable is required")};argBytes:=0;for _,arg:=range q.Argv{argBytes+=len(arg);if strings.ContainsRune(arg,0){return nil,ErrDenied}};if argBytes>64<<10{return nil,ErrLimit}
 allowed:=probe;for _,exe:=range c.Executables{if exe==q.Argv[0]{allowed=true}};if !allowed{return nil,fmt.Errorf("%w: executable not in operator manifest",ErrDenied)}
 if q.Timeout==0{if q.Kind==Command{q.Timeout=60*time.Second}else if q.Kind==Voice{q.Timeout=180*time.Second}}
 if q.Timeout<0||q.Timeout>0&&q.Timeout<time.Second||q.Kind==Command&&q.Timeout>300*time.Second||q.Kind==Voice&&q.Timeout>180*time.Second{return nil,fmt.Errorf("invalid worker timeout")}
 if !probe{if e:=q.Workspace.acquireWorker();e!=nil{return nil,e};defer func(){if err!=nil{q.Workspace.releaseWorker()}}()}
 select{case workers<-struct{}{}:default:return nil,fmt.Errorf("%w: maximum four concurrent workers",ErrLimit)}
 defer func(){if err!=nil{<-workers}}()
 cg,e:=newCgroup(c.Limits,q.Kind);if e!=nil{return nil,e};defer func(){if err!=nil{cg.close()}}()
 var workerCtx context.Context
 var cancel context.CancelFunc
 if q.Timeout>0{workerCtx,cancel=context.WithTimeout(ctx,q.Timeout)}else{workerCtx,cancel=context.WithCancel(ctx)}
 p=&Prepared{cg:cg,ctx:workerCtx,cancel:cancel,done:make(chan struct{}),closeDone:make(chan struct{}),reserved:true,beforeStart:q.BeforeStart,config:c};prepared:=p;defer func(){if err!=nil{cancel();for _,dir:=range prepared.cleanup{removePrivate(dir)}}}()
 if !probe{p.workspace=q.Workspace}
 args:=[]string{"--unshare-user","--unshare-pid","--unshare-ipc","--unshare-net","--unshare-uts","--die-with-parent","--new-session","--cap-drop","ALL","--clearenv","--tmpfs","/","--proc","/proc","--dev","/dev","--tmpfs","/tmp","--dir","/home","--dir","/home/worker","--setenv","HOME","/home/worker","--setenv","PATH","/usr/bin:/bin","--setenv","LANG","C.UTF-8"}
 mounted:=[]string{};for _,a:=range c.Manifest{covered:=false;for _,parent:=range mounted{if strings.HasPrefix(a.Path,parent+"/"){covered=true;break}};if covered{continue};args=append(args,"--ro-bind",a.Path,a.Path);mounted=append(mounted,a.Path)}
 mount:="--bind";if q.ReadOnly{mount="--ro-bind"};args=append(args,mount,q.Workspace.Root,"/workspace","--chdir","/workspace")
 if q.State!=nil{if q.Kind!=ACP&&q.Kind!=Browser{return nil,ErrDenied};if q.State.store!=q.Workspace.store||q.State.path==""{return nil,ErrDenied};target:="/home/worker/browser-data";if q.Kind==ACP{target="/home/worker"};args=append(args,"--bind",q.State.path,target)}
 auth,e:=p.authMounts(q);if e!=nil{return nil,e};args=append(args,auth...)
 for _,s:=range q.SocketMounts{
  if q.Kind!=Browser&&q.Kind!=ACP{cancel();return nil,ErrDenied};if s.Purpose=="egress"{if s.Target!="/run/meshra/egress.sock"{cancel();return nil,ErrDenied}}else if s.Purpose=="broker"{if q.Kind!=ACP||s.Target!="/run/meshra/broker.sock"{cancel();return nil,ErrDenied}}else{cancel();return nil,ErrDenied};st,e:=os.Lstat(s.Source);if e!=nil||st.Mode()&os.ModeSocket==0{cancel();return nil,fmt.Errorf("purpose-limited Unix socket required")};args=append(args,"--ro-bind",s.Source,s.Target)
 }
 for name,value:=range q.Env{
  if name==""||name=="HOME"||name=="PATH"||name=="DISPLAY"||name=="WAYLAND_DISPLAY"||name=="XAUTHORITY"||name=="DBUS_SESSION_BUS_ADDRESS"||name=="MESHRA_ADMIN_TOKEN"||name=="MESHRA_TOKEN"||strings.HasPrefix(name,"LD_")||strings.HasPrefix(name,"BASH")||name=="ENV"||name=="NODE_OPTIONS"||strings.ContainsAny(name,"=\x00")||strings.ContainsRune(value,0){return nil,ErrDenied}
  if q.Kind==Browser&&name!="PLAYWRIGHT_BROWSERS_PATH"&&name!="PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD"&&name!="NODE_PATH"{return nil,ErrDenied}
  args=append(args,"--setenv",name,value)
 }
 args=append(args,"--");args=append(args,q.Argv...)
 cmd:=exec.Command(c.Executables["bwrap"],args...);cmd.Env=[]string{"PATH=/usr/bin:/bin"};cmd.Dir="/";cmd.SysProcAttr=&syscall.SysProcAttr{Pdeathsig:syscall.SIGKILL,UseCgroupFD:true,CgroupFD:int(cg.fd.Fd())}
 p.Cmd=cmd;p.executable=cmd.Path;p.sealedArgs=append([]string(nil),cmd.Args...);return p,nil
}
func(p *Prepared)Start()error{
 p.mu.Lock()
 defer p.mu.Unlock()
 if p.closed||p.started{return fmt.Errorf("worker already started or closed")}
 if e:=p.ctx.Err();e!=nil{return e}
 if e:=Validate(p.config);e!=nil{return e}
 if p.beforeStart!=nil{
  if e:=p.beforeStart();e!=nil{return e}
 }
 // Cmd is exposed for pipe wiring only. Never permit a transport helper to
 // turn this prepared launch into a host process or inherit daemon secrets.
 if p.Cmd.Path!=p.executable||p.Cmd.Dir!="/"||!slices.Equal(p.Cmd.Args,p.sealedArgs)||len(p.Cmd.Env)!=1||p.Cmd.Env[0]!="PATH=/usr/bin:/bin"||len(p.Cmd.ExtraFiles)!=0{return fmt.Errorf("%w: prepared launcher fields were modified",ErrDenied)}
 attributes:=syscall.SysProcAttr{Pdeathsig:syscall.SIGKILL,UseCgroupFD:true,CgroupFD:int(p.cg.fd.Fd())}
 if p.Cmd.SysProcAttr==nil{p.Cmd.SysProcAttr=&attributes}else{*p.Cmd.SysProcAttr=attributes}
 if e:=p.ctx.Err();e!=nil{return e}
 // UseCgroupFD sets CLONE_INTO_CGROUP. Unsupported clone3 fails here; there is
 // deliberately no fork-then-move or unconstrained fallback.
 if e:=p.Cmd.Start();e!=nil{
  return fmt.Errorf("%w: clone3 born-in-cgroup/bubblewrap start: %v",ErrUnavailable,e)
 }
 p.started=true
 go func(){
  select{
  case <-p.ctx.Done():
   p.cg.kill()
   if p.Cmd.Process!=nil{p.Cmd.Process.Kill()}
  case <-p.done:
  }
 }()
 return nil
}
func(p *Prepared)Wait()error{
 p.mu.Lock()
 started:=p.started
 p.mu.Unlock()
 if !started{return fmt.Errorf("worker not started")}
 p.waitOnce.Do(func(){
  e:=p.Cmd.Wait()
  p.cg.kill()
  p.mu.Lock()
  p.waitErr=e
  close(p.done)
  p.mu.Unlock()
 })
 <-p.done
 p.mu.Lock()
 defer p.mu.Unlock()
 return p.waitErr
}
func(p *Prepared)Close()error{
 p.closeOnce.Do(func(){
  p.mu.Lock()
  p.closed=true
  p.cancel()
  started:=p.started
  p.mu.Unlock()
  p.cg.kill()
  if started{p.Wait()}
  cgroupErr:=p.cg.close()
  p.closeErr=cgroupErr
  for _,dir:=range p.cleanup{p.closeErr=errors.Join(p.closeErr,removePrivate(dir))}
  if cgroupErr==nil&&p.workspace!=nil{p.workspace.releaseWorker()}
  p.mu.Lock()
  if cgroupErr==nil&&p.reserved{<-workers;p.reserved=false}
  p.mu.Unlock()
  close(p.closeDone)
 })
 <-p.closeDone
 return p.closeErr
}

type boundedOutput struct{mu sync.Mutex;remaining int;out,err bytes.Buffer;truncated bool}
type outputWriter struct{b *boundedOutput;stderr bool}
func(w outputWriter)Write(v []byte)(int,error){w.b.mu.Lock();defer w.b.mu.Unlock();n:=min(w.b.remaining,len(v));if w.stderr{w.b.err.Write(v[:n])}else{w.b.out.Write(v[:n])};w.b.remaining-=n;if n<len(v){w.b.truncated=true};return len(v),nil}
func Run(ctx context.Context,c Config,q Request)(result Result,err error){
 p,e:=Prepare(ctx,c,q)
 if e!=nil{return result,e}
 defer func(){err=errors.Join(err,p.Close())}()
 output:=&boundedOutput{remaining:256<<10}
 p.Cmd.Stdout=outputWriter{b:output}
 p.Cmd.Stderr=outputWriter{b:output,stderr:true}
 if e=p.Start();e!=nil{return result,e}
 e=p.Wait()
 result.ExitCode=p.Cmd.ProcessState.ExitCode()
 result.Stdout=output.out.String()
 result.Stderr=output.err.String()
 result.Truncated=output.truncated
 if p.ctx.Err()!=nil{return result,p.ctx.Err()}
 var exit *exec.ExitError
 if e!=nil&&!errors.As(e,&exit){return result,e}
 if !q.ReadOnly{result.Changes,e=q.Workspace.Changes(ctx)}
 return result,e
}
var _ io.Writer = outputWriter{}
