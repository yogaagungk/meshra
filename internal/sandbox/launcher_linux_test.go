//go:build linux

package sandbox

import (
 "context"
 "errors"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "testing"
 "time"
)

func TestMissingConfinementNeverExecutesOnHost(t *testing.T){
 root:=t.TempDir();s:=snapshotTest(t,root);sentinel:=filepath.Join(root,"must-not-exist")
 _,e:=Run(context.Background(),Config{},Request{Workspace:s,Argv:[]string{"/bin/sh","-c","touch "+sentinel},Kind:Command});if !errors.Is(e,ErrUnavailable){t.Fatalf("missing prerequisite: %v",e)};if _,e=os.Stat(sentinel);!os.IsNotExist(e){t.Fatal("host fallback ran")}
}
func TestPinnedRuntimeAssetChangeRefusesExecution(t *testing.T){
 // A selected non-system asset is an actual local file, not a worker substitute.
 home,e:=os.UserHomeDir();if e!=nil{t.Fatal(e)};dir,e:=os.MkdirTemp(home,"meshra-manifest-test-");if e!=nil{t.Fatal(e)};defer os.RemoveAll(dir);asset:=filepath.Join(dir,"asset");writeTestFile(t,asset,"pinned")
 c,e:=Configure(Config{RuntimePaths:[]string{asset}});if e!=nil{t.Fatal(e)};writeTestFile(t,asset,"changed");if e=Validate(c);!errors.Is(e,ErrUnavailable){t.Fatalf("changed asset accepted: %v",e)}
}
func realConfig(t *testing.T)Config{t.Helper();bwrap,e:=exec.LookPath("bwrap");if e!=nil{t.Skip("missing real bubblewrap executable; no host fallback")};shell,e:=filepath.EvalSymlinks("/bin/sh");if e!=nil{t.Fatal(e)};c,e:=Configure(Config{Executables:map[string]string{"bwrap":bwrap,"shell":shell}});if e!=nil{t.Fatal(e)};status:=Check(context.Background(),c);if status.State!="ready"{t.Skipf("real confinement prerequisites unavailable: %v",status.Prerequisites)};return c}
func TestRealWorkerPrivateRootCredentialsAndOutsideWrite(t *testing.T){
 c:=realConfig(t);root:=t.TempDir();s:=snapshotTest(t,root);outside:=filepath.Join(t.TempDir(),"sentinel");writeTestFile(t,outside,"unchanged");t.Setenv("MESHRA_ADMIN_TOKEN","must-not-be-inherited");t.Setenv("OPENAI_API_KEY","must-not-be-inherited")
 script:=`test -z "$MESHRA_ADMIN_TOKEN$OPENAI_API_KEY" && test "$HOME" = /home/worker && test ! -e /run/user && test ! -e /mnt/c && test ! -e /etc/passwd && test ! -e /sys/fs/cgroup && ! test -e `+outside+` && printf private > /workspace/result && ! printf escape > `+outside
 result,e:=Run(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c",script},Kind:Command});if e!=nil{t.Fatal(e)};if result.ExitCode!=0{t.Fatalf("isolation failed: %+v",result)};defer result.Changes.Close();if _,e=os.Stat(filepath.Join(root,"result"));!os.IsNotExist(e){t.Fatal("worker wrote original workspace")};b,_:=os.ReadFile(outside);if string(b)!="unchanged"{t.Fatal("outside sentinel changed")}
}
func TestRealWorkerCancellationKillsSetsidDescendants(t *testing.T){
 c:=realConfig(t);s:=snapshotTest(t,t.TempDir());ctx,cancel:=context.WithCancel(context.Background());p,e:=Prepare(ctx,c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","setsid sh -c 'while :; do sleep 1; done' & wait"},Kind:Command});if e!=nil{t.Fatal(e)};defer p.Close();if e=p.Start();e!=nil{t.Fatal(e)};cancel();done:=make(chan error,1);go func(){done<-p.Wait()}();select{case <-done:case <-time.After(5*time.Second):t.Fatal("worker survived cancellation")};b,e:=os.ReadFile(filepath.Join(p.cg.dir,"cgroup.events"));if e!=nil{t.Fatal(e)};if !strings.Contains(string(b),"populated 0"){t.Fatalf("descendant survived: %s",b)}
}
func TestRealWorkerAggregateOutputAndDispatchClaim(t *testing.T){
 c:=realConfig(t);s:=snapshotTest(t,t.TempDir());claimed:=false;result,e:=Run(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","yes x | head -c 200000; yes y | head -c 200000 >&2"},Kind:Command,BeforeStart:func()error{claimed=true;return nil}});if e!=nil{t.Fatal(e)};defer result.Changes.Close();if !claimed||!result.Truncated||len(result.Stdout)+len(result.Stderr)!=256<<10{t.Fatalf("aggregate output: %d+%d claimed=%v truncated=%v",len(result.Stdout),len(result.Stderr),claimed,result.Truncated)}
 p,e:=Prepare(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","touch /workspace/denied"},Kind:Command,BeforeStart:func()error{return ErrDenied}});if e!=nil{t.Fatal(e)};defer p.Close();if e=p.Start();!errors.Is(e,ErrDenied){t.Fatal(e)};if _,e=os.Stat(filepath.Join(s.Root,"denied"));!os.IsNotExist(e){t.Fatal("revoked action started")}
}

func TestRealWorkerCgroupLimitsConcurrencyAndSnapshotOwnership(t *testing.T){
 c:=realConfig(t);s:=snapshotTest(t,t.TempDir())
 for _,kind:=range []Kind{Command,Browser,Voice}{
  p,e:=Prepare(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","exit 0"},Kind:kind,ReadOnly:true});if e!=nil{t.Fatal(e)}
  memory,pids:="1073741824","128";if kind!=Command{memory,pids="2147483648","256"}
  for file,expected:=range map[string]string{"memory.max":memory,"pids.max":pids,"memory.swap.max":"0","memory.oom.group":"1"}{b,e:=os.ReadFile(filepath.Join(p.cg.dir,file));if e!=nil||strings.TrimSpace(string(b))!=expected{p.Close();t.Fatalf("unenforced %s: %q %v",file,b,e)}}
  if e=s.Close();!errors.Is(e,ErrConflict){p.Close();t.Fatalf("live worker snapshot was deleted: %v",e)};if e=p.Close();e!=nil{t.Fatal(e)}
 }
 prepared:=[]*Prepared{};defer func(){for _,p:=range prepared{p.Close()}}()
 for range 4{p,e:=Prepare(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","exit 0"},Kind:Command,ReadOnly:true});if e!=nil{t.Fatal(e)};prepared=append(prepared,p)}
 if p,e:=Prepare(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","exit 0"},Kind:Command,ReadOnly:true});!errors.Is(e,ErrLimit){if p!=nil{p.Close()};t.Fatalf("fifth worker accepted: %v",e)}
}
