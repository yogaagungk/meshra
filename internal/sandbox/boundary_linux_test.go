//go:build linux

package sandbox

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "testing"
 "time"

 "golang.org/x/sys/unix"
)

func TestSnapshotRejectsSameDeviceNestedBindMount(t *testing.T){
 if os.Getenv("MESHRA_SANDBOX_MOUNT_CHILD")=="1"{
  root,outside,store:=os.Getenv("MESHRA_TEST_ROOT"),os.Getenv("MESHRA_TEST_OUTSIDE"),os.Getenv("MESHRA_TEST_STORE")
  if e:=unix.Mount(outside,filepath.Join(root,"nested"),"",unix.MS_BIND,"");e!=nil{t.Fatal(e)};defer unix.Unmount(filepath.Join(root,"nested"),0)
  s,e:=SnapshotWorkspace(context.Background(),root,store,Limits{});if s!=nil{s.Close()};if !errors.Is(e,ErrDenied){t.Fatalf("same-device nested mount accepted: %v",e)};return
 }
 unshare,e:=exec.LookPath("unshare");if e!=nil{t.Skip("unshare executable unavailable for real mount-boundary test")}
 probe:=exec.Command(unshare,"--user","--map-root-user","--mount","/usr/bin/true");if b,e:=probe.CombinedOutput();e!=nil{t.Skipf("user/mount namespace prerequisite unavailable: %v: %s",e,b)}
 root,outside,store:=t.TempDir(),t.TempDir(),t.TempDir();if e=os.Mkdir(filepath.Join(root,"nested"),0700);e!=nil{t.Fatal(e)};writeTestFile(t,filepath.Join(outside,"sentinel"),"outside")
 child:=exec.Command(unshare,"--user","--map-root-user","--mount",os.Args[0],"-test.run=^TestSnapshotRejectsSameDeviceNestedBindMount$");child.Env=append(os.Environ(),"MESHRA_SANDBOX_MOUNT_CHILD=1","MESHRA_TEST_ROOT="+root,"MESHRA_TEST_OUTSIDE="+outside,"MESHRA_TEST_STORE="+store);if b,e:=child.CombinedOutput();e!=nil{t.Fatalf("real nested bind test failed: %v: %s",e,b)}
}

func TestRealWorkerDiesWithDaemonIncludingDetachedChildren(t *testing.T){
 if raw:=os.Getenv("MESHRA_SANDBOX_DAEMON_CHILD");raw!=""{
  var c Config;if e:=json.Unmarshal([]byte(raw),&c);e!=nil{t.Fatal(e)}
  s,e:=SnapshotWorkspace(context.Background(),os.Getenv("MESHRA_TEST_ROOT"),os.Getenv("MESHRA_TEST_STORE"),c.Limits);if e!=nil{t.Fatal(e)};defer s.Close()
  p,e:=Prepare(context.Background(),c,Request{Workspace:s,Argv:[]string{c.Executables["shell"],"-c","printf ready > /workspace/started; setsid sh -c 'while :; do sleep 1; done' & wait"},Kind:Command,Timeout:300*time.Second});if e!=nil{t.Fatal(e)};defer p.Close();p.Cmd.Stdout=io.Discard;p.Cmd.Stderr=io.Discard;if e=p.Start();e!=nil{t.Fatal(e)}
  for deadline:=time.Now().Add(5*time.Second);;{if _,e=os.Stat(filepath.Join(s.Root,"started"));e==nil{break};if time.Now().After(deadline){t.Fatal("worker never started")};time.Sleep(10*time.Millisecond)}
  fmt.Println(p.cg.dir)
  time.Sleep(60*time.Second)
  t.Fatal("daemon-death test parent never killed helper")
 }
 c:=realConfig(t);if _,e:=exec.LookPath("setsid");e!=nil{t.Skip("setsid executable required for real detached-child test")};encoded,_:=json.Marshal(c)
 child:=exec.Command(os.Args[0],"-test.run=^TestRealWorkerDiesWithDaemonIncludingDetachedChildren$");child.Env=append(os.Environ(),"MESHRA_SANDBOX_DAEMON_CHILD="+string(encoded),"MESHRA_TEST_ROOT="+t.TempDir(),"MESHRA_TEST_STORE="+t.TempDir());pipe,e:=child.StdoutPipe();if e!=nil{t.Fatal(e)};child.Stderr=os.Stderr;if e=child.Start();e!=nil{t.Fatal(e)};defer func(){child.Process.Kill();child.Wait()}()
 // Read one bounded line; a killed daemon must not keep this pipe open through
 // descendants because the worker receives only explicit stdout/stderr handles.
 type response struct{path string;err error};ready:=make(chan response,1);go func(){var b strings.Builder;var one [1]byte;for b.Len()<4096{_,e:=pipe.Read(one[:]);if e!=nil{ready<-response{err:e};return};if one[0]=='\n'{ready<-response{path:b.String()};return};b.WriteByte(one[0])};ready<-response{err:ErrLimit}}()
 var cg string;select{case value:=<-ready:if value.err!=nil{t.Fatal(value.err)};cg=value.path;case <-time.After(10*time.Second):t.Fatal("confined helper readiness timed out")};defer os.Remove(cg)
 if !strings.HasPrefix(filepath.Base(cg),"meshra-worker-"){t.Fatalf("unexpected cgroup path %q",cg)};if e=child.Process.Kill();e!=nil{t.Fatal(e)};child.Wait()
 deadline:=time.Now().Add(5*time.Second);for{b,e:=os.ReadFile(filepath.Join(cg,"cgroup.events"));if e!=nil{t.Fatal(e)};if strings.Contains(string(b),"populated 0"){break};if time.Now().After(deadline){t.Fatalf("detached children survived daemon death: %s",b)};time.Sleep(10*time.Millisecond)}
}
