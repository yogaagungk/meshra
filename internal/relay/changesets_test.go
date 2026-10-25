package relay

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "strings"
 "testing"

 "meshra/internal/model"
 "meshra/internal/sandbox"
)

func changesetFixture(t *testing.T)(*Relay,model.Principal,model.Workspace,string){
 t.Helper();r,_:=openTest(t,nil);p:=model.Principal{Admin:true,ConsoleSessionID:"changeset-session"};r.SetConsoleSessionValidator(func(id string)bool{_,e:=r.Agents(admin);return id==p.ConsoleSessionID&&e==nil})
 root:=t.TempDir();v,e:=r.workspaceOperation("workspaces/add",model.ConsoleRequest{Path:root});if e!=nil{t.Fatal(e)};w:=v.(model.Workspace)
 from:=createTest(t,r,"omp","changeset-source");to:=createTest(t,r,"codex","changeset-dest");task,e:=r.Send(admin,model.SendRequest{From:from.ID,To:to.ID,Text:"stage exact private changes",IdempotencyKey:"changeset-fixture"});if e!=nil{t.Fatal(e)};id:=randomID();if _,e=r.db.Exec("INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES(?,?,1,'call','action','completed','{}')",id,task.ID);e!=nil{t.Fatal(e)};return r,p,w,id
}
func TestChangesetConsoleApplyRevisionAndPrivatePaths(t *testing.T){
 r,p,w,invocation:=changesetFixture(t);original:=filepath.Join(w.Path,"file");if e:=os.WriteFile(original,[]byte("old"),0644);e!=nil{t.Fatal(e)};s,e:=r.prepareWorkspace(context.Background(),w.ID);if e!=nil{t.Fatal(e)};defer s.Close();m,e:=s.Stage("file",[]byte("new"),0644);if e!=nil{t.Fatal(e)};defer m.Close();v,e:=r.persistChangeset(context.Background(),invocation,w.ID,m);if e!=nil{t.Fatal(e)};body,_:=json.Marshal(v);if strings.Contains(string(body),r.sandboxStore())||strings.Contains(string(body),"Blob"){t.Fatal("changeset leaked private storage paths")};b,_:=os.ReadFile(original);if string(b)!="old"{t.Fatal("staging changed original")}
 _,e=r.applyChangeset(context.Background(),admin,v.ID,v.ManifestHash,v.Revision,nil);requireStatus(t,e,403);_,e=r.applyChangeset(context.Background(),p,v.ID,"wrong",v.Revision,nil);requireStatus(t,e,409)
 applied,e:=r.applyChangeset(context.Background(),p,v.ID,v.ManifestHash,v.Revision,nil);if e!=nil{t.Fatal(e)};if applied.State!="applied"{t.Fatal(applied.State)};b,_=os.ReadFile(original);if string(b)!="new"{t.Fatal("approved exact change not applied")};var journal string;if e=r.db.QueryRow("SELECT state FROM workspace_apply_journal WHERE changeset_id=? AND path='file'",v.ID).Scan(&journal);e!=nil||journal!="applied"{t.Fatalf("journal %q: %v",journal,e)}
 _,e=r.applyChangeset(context.Background(),p,v.ID,v.ManifestHash,applied.Revision,nil);requireStatus(t,e,409)
}
func TestChangesetRevalidatesConsoleAndReportsPartialApply(t *testing.T){
 r,p,w,invocation:=changesetFixture(t);for _,name:=range []string{"a","b"}{if e:=os.WriteFile(filepath.Join(w.Path,name),[]byte("old"),0644);e!=nil{t.Fatal(e)}};s,e:=r.prepareWorkspace(context.Background(),w.ID);if e!=nil{t.Fatal(e)};defer s.Close();for _,name:=range []string{"a","b"}{if e=os.WriteFile(filepath.Join(s.Root,name),[]byte("new"),0644);e!=nil{t.Fatal(e)}};m,e:=s.Changes(context.Background());if e!=nil{t.Fatal(e)};v,e:=r.persistChangeset(context.Background(),invocation,w.ID,m);if e!=nil{t.Fatal(e)}
 claim:=func()error{var n int;if e:=r.db.QueryRow("SELECT count(*) FROM workspace_apply_journal WHERE changeset_id=? AND path='a' AND state='applied'",v.ID).Scan(&n);e!=nil{return e};if n!=0{r.RevokeConsoleSession(p.ConsoleSessionID);return r.requireConsole(p)};return nil}
 partial,e:=r.applyChangeset(context.Background(),p,v.ID,v.ManifestHash,v.Revision,claim);if e==nil||partial.State!="partial"||!strings.Contains(e.Error(),"changed=[a]"){t.Fatalf("partial result: %+v %v",partial,e)};a,_:=os.ReadFile(filepath.Join(w.Path,"a"));b,_:=os.ReadFile(filepath.Join(w.Path,"b"));if string(a)!="new"||string(b)!="old"{t.Fatalf("partial bytes %q %q",a,b)}
}
func TestRuntimeConfigurationRevisionAndNoImplicitReadiness(t *testing.T){
 r,_:=openTest(t,nil);_,e:=r.runtimeOperation(context.Background(),model.Principal{AgentID:"agent"},"runtime.configure",json.RawMessage(`{"expectedRevision":0,"executables":{},"runtimePaths":[],"limits":{}}`));requireStatus(t,e,403)
 raw:=json.RawMessage(`{"expectedRevision":0,"executables":{},"runtimePaths":[],"limits":{}}`);v,e:=r.runtimeOperation(context.Background(),admin,"runtime.configure",raw);if e!=nil{t.Fatal(e)};settings:=v.(runtimeSettings);if settings.Revision!=1||settings.Limits.StoreBytes!=2<<30{t.Fatalf("settings %+v",settings)}
 _,e=r.runtimeOperation(context.Background(),admin,"runtime.configure",raw);requireStatus(t,e,409);if status:=sandbox.Check(context.Background(),settings.Config);status.State=="ready"{t.Fatal("missing bubblewrap reported ready")}
}

func TestChangesetQueriesExposeDurableJournalAndInterruptedRecovery(t *testing.T){
 r,p,w,invocation:=changesetFixture(t);if e:=os.WriteFile(filepath.Join(w.Path,"file"),[]byte("old"),0644);e!=nil{t.Fatal(e)}
 s,e:=r.prepareWorkspace(context.Background(),w.ID);if e!=nil{t.Fatal(e)};defer s.Close();m,e:=s.Stage("file",[]byte("new"),0);if e!=nil{t.Fatal(e)};v,e:=r.persistChangeset(context.Background(),invocation,w.ID,m);if e!=nil{t.Fatal(e)}
 r.mu.Lock();v.State="applying";v.Revision++;e=r.storeChangeset(v);r.mu.Unlock();if e!=nil{t.Fatal(e)}
 if _,e=r.db.Exec("INSERT INTO workspace_apply_journal VALUES(?,?,'applying',?,?,'',?)",v.ID,"file",m.Changes[0].Before.Hash,m.Changes[0].After.Hash,now().Format("2006-01-02T15:04:05.999999999Z07:00"));e!=nil{t.Fatal(e)}
 r.mu.Lock();e=r.cleanupSandboxStore();r.mu.Unlock();if e!=nil{t.Fatal(e)}
 raw,_:=json.Marshal(map[string]string{"id":v.ID});_,e=r.changesetOperation(context.Background(),admin,"changesets.get",raw);requireStatus(t,e,403)
 result,e:=r.changesetOperation(context.Background(),p,"changesets.get",raw);if e!=nil{t.Fatal(e)};detail:=result.(workspaceChangesetDetail)
 if detail.State!="uncertain"||detail.Revision!=v.Revision+1||len(detail.Journal)!=1||detail.Journal[0].State!="applying"{t.Fatalf("recovery lost journal: %+v",detail)}
 var revision int;if e=r.db.QueryRow("SELECT revision FROM workspace_changesets WHERE id=?",v.ID).Scan(&revision);e!=nil||revision!=detail.Revision{t.Fatalf("persisted revision mismatch: %d %v",revision,e)}
 list,e:=r.changesetOperation(context.Background(),p,"changesets.list",json.RawMessage(`{"limit":1}`));if e!=nil{t.Fatal(e)};body,_:=json.Marshal(list);if !strings.Contains(string(body),v.ID)||strings.Contains(string(body),"preview")||strings.Contains(string(body),r.sandboxStore()){t.Fatalf("unsafe/incomplete summary: %s",body)}
 _,e=r.applyChangeset(context.Background(),p,v.ID,v.ManifestHash,detail.Revision,nil);requireStatus(t,e,409)
 _,e=r.changesetOperation(context.Background(),p,"changesets.list",json.RawMessage(`{"limit":101}`));requireStatus(t,e,400)
}
