//go:build linux

package sandbox

import (
 "context"
 "errors"
 "os"
 "path/filepath"
 "strings"
 "testing"

 "golang.org/x/sys/unix"
)

func writeTestFile(t *testing.T,p,text string){t.Helper();if e:=os.WriteFile(p,[]byte(text),0644);e!=nil{t.Fatal(e)}}
func snapshotTest(t *testing.T,workspace string)*Snapshot{t.Helper();s,e:=SnapshotWorkspace(context.Background(),workspace,t.TempDir(),Limits{});if e!=nil{t.Fatal(e)};t.Cleanup(func(){s.Close()});return s}
func TestPrivateSnapshotCopiesHardlinksAndOmitsGit(t *testing.T){
 root:=t.TempDir();outside:=filepath.Join(t.TempDir(),"sentinel");writeTestFile(t,outside,"outside")
 if e:=os.Link(outside,filepath.Join(root,"linked"));e!=nil{t.Fatal(e)};if e:=os.Mkdir(filepath.Join(root,".git"),0700);e!=nil{t.Fatal(e)};writeTestFile(t,filepath.Join(root,".git","config"),"protected")
 s:=snapshotTest(t,root);writeTestFile(t,filepath.Join(s.Root,"linked"),"private only");b,e:=os.ReadFile(outside);if e!=nil||string(b)!="outside"{t.Fatalf("outside inode changed: %q %v",b,e)};if _,e=os.Stat(filepath.Join(s.Root,".git"));!os.IsNotExist(e){t.Fatal("git metadata copied")}
}
func TestWorkspaceRejectsOutsideLinksDevicesAndBounds(t *testing.T){
 for _,kind:=range []string{"symlink","fifo","bytes","files"}{t.Run(kind,func(t *testing.T){root:=t.TempDir();limits:=Limits{SnapshotBytes:4,SnapshotFiles:1,StoreBytes:1<<20};switch kind{case "symlink":if e:=os.Symlink(filepath.Join(t.TempDir(),"outside"),filepath.Join(root,"bad"));e!=nil{t.Fatal(e)};case "fifo":if e:=unix.Mkfifo(filepath.Join(root,"fifo"),0600);e!=nil{t.Fatal(e)};case "bytes":writeTestFile(t,filepath.Join(root,"large"),"12345");case "files":writeTestFile(t,filepath.Join(root,"a"),"1");writeTestFile(t,filepath.Join(root,"b"),"2")};if s,e:=SnapshotWorkspace(context.Background(),root,t.TempDir(),limits);e==nil{s.Close();t.Fatal("unsafe/oversized snapshot accepted")}})}
}
func TestInternalSymlinksAndBoundedRootedReads(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"file"),"abcdef");if e:=os.Symlink("file",filepath.Join(root,"internal"));e!=nil{t.Fatal(e)};s:=snapshotTest(t,root);if v,e:=os.Readlink(filepath.Join(s.Root,"internal"));e!=nil||v!="file"{t.Fatalf("internal link: %q %v",v,e)}
 b,e:=ReadWorkspace(root,"file",2,3);if e!=nil||string(b)!="cde"{t.Fatalf("bounded read: %q %v",b,e)}
 for _,path:=range []string{"../file",".git/config","internal"}{if _,e=ReadWorkspace(root,path,0,32);e==nil{t.Fatalf("read accepted %q",path)}};if _,e=s.Read("file",0,32,false);!errors.Is(e,ErrDenied){t.Fatal(e)}
}
func TestExactChangesetCreatesParentsAndReplacesHardlinkEntry(t *testing.T){
 root:=t.TempDir();outside:=filepath.Join(t.TempDir(),"outside");writeTestFile(t,outside,"original");if e:=os.Link(outside,filepath.Join(root,"file"));e!=nil{t.Fatal(e)}
 s:=snapshotTest(t,root);m,e:=s.Stage("file",[]byte("replacement"),0644);if e!=nil{t.Fatal(e)};m.Close();m,e=s.Stage("new/sub/file",[]byte("new"),0644);if e!=nil{t.Fatal(e)};defer m.Close();if e=m.Save();e!=nil{t.Fatal(e)};loaded,e:=LoadManifest(m.Directory(),m.Hash);if e!=nil{t.Fatal(e)}
 applied:=map[string]bool{};e=loaded.Apply(context.Background(),root,func()error{return nil},func(r ApplyRecord)error{if r.State=="applied"{applied[r.Path]=true};return nil});if e!=nil{t.Fatal(e)};if len(applied)!=4{t.Fatalf("journal: %v",applied)};b,_:=os.ReadFile(outside);if string(b)!="original"{t.Fatal("apply wrote original hardlink inode")};b,_=os.ReadFile(filepath.Join(root,"new/sub/file"));if string(b)!="new"{t.Fatal("parent creation lost output")}
}
func TestChangesetRejectsStaleOriginalAndChangedBlobs(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"file"),"original");s:=snapshotTest(t,root);m,e:=s.Stage("file",[]byte("new"),0644);if e!=nil{t.Fatal(e)};defer m.Close();writeTestFile(t,filepath.Join(root,"file"),"external change")
 claimed:=false;e=m.Apply(context.Background(),root,func()error{claimed=true;return nil},func(ApplyRecord)error{return nil});if !errors.Is(e,ErrConflict)||claimed{t.Fatalf("stale apply: %v, claimed=%v",e,claimed)}
 writeTestFile(t,filepath.Join(root,"file"),"original");if e=os.Chmod(m.Changes[0].Blob,0600);e!=nil{t.Fatal(e)};writeTestFile(t,m.Changes[0].Blob,"tampered");if e=m.Apply(context.Background(),root,func()error{return nil},func(ApplyRecord)error{return nil});!errors.Is(e,ErrConflict){t.Fatalf("blob mutation accepted: %v",e)}
}
func TestChangesetPartialJournalAndClaimRevocation(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"a"),"old");writeTestFile(t,filepath.Join(root,"b"),"old");s:=snapshotTest(t,root);writeTestFile(t,filepath.Join(s.Root,"a"),"new");writeTestFile(t,filepath.Join(s.Root,"b"),"new");m,e:=s.Changes(context.Background());if e!=nil{t.Fatal(e)};defer m.Close()
 revoked:=false;records:=map[string]string{};e=m.Apply(context.Background(),root,func()error{if revoked{return ErrDenied};return nil},func(r ApplyRecord)error{records[r.Path]=r.State;if r.Path=="a"&&r.State=="applied"{revoked=true};return nil});if !errors.Is(e,ErrDenied){t.Fatal(e)};a,_:=os.ReadFile(filepath.Join(root,"a"));b,_:=os.ReadFile(filepath.Join(root,"b"));if string(a)!="new"||string(b)!="old"||records["a"]!="applied"{t.Fatalf("inaccurate partial effects: %q %q %v",a,b,records)}
}
func TestChangesetRejectsOutputSymlinksAndProtectedPaths(t *testing.T){for _,name:=range []string{"link",".git"}{t.Run(name,func(t *testing.T){root:=t.TempDir();s:=snapshotTest(t,root);if name=="link"{if e:=os.Symlink("target",filepath.Join(s.Root,name));e!=nil{t.Fatal(e)}}else{if e:=os.Mkdir(filepath.Join(s.Root,name),0700);e!=nil{t.Fatal(e)}};if m,e:=s.Changes(context.Background());e==nil{m.Close();t.Fatal("unsafe output accepted")}})}}
func TestStoreQuotaAndManifestPreviewBound(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"file"),strings.Repeat("a",70000));store:=t.TempDir();limits:=Limits{SnapshotBytes:100000,SnapshotFiles:10,StoreBytes:200000};s,e:=SnapshotWorkspace(context.Background(),root,store,limits);if e!=nil{t.Fatal(e)};defer s.Close();if second,e:=SnapshotWorkspace(context.Background(),root,store,limits);e!=nil{t.Fatal(e)}else{defer second.Close()};if third,e:=SnapshotWorkspace(context.Background(),root,store,limits);!errors.Is(e,ErrLimit){if third!=nil{third.Close()};t.Fatalf("store quota ignored: %v",e)}
 m,e:=s.Stage("file",[]byte(strings.Repeat("b",70000)),0644);if e==nil{defer m.Close();if len(m.Preview)>64<<10||!m.PreviewTruncated{t.Fatal("unbounded/misleading preview")}}else if !errors.Is(e,ErrLimit){t.Fatal(e)}
}

func TestPreviewStagePreservesPriorWorkingChangesAndMatchesApprovedHash(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"first"),"baseline");writeTestFile(t,filepath.Join(root,"second"),"baseline");s:=snapshotTest(t,root);writeTestFile(t,filepath.Join(s.Root,"first"),"previously approved")
 preview,e:=s.PreviewStage("new/sub/file",[]byte("prospective"),0644);if e!=nil{t.Fatal(e)};defer preview.Close();if _,e=os.Stat(filepath.Join(s.Root,"new"));!os.IsNotExist(e){t.Fatal("preview mutated working snapshot")};b,_:=os.ReadFile(filepath.Join(s.Root,"first"));if string(b)!="previously approved"{t.Fatal("preview discarded prior private changes")}
 actual,e:=s.Stage("new/sub/file",[]byte("prospective"),0644);if e!=nil{t.Fatal(e)};defer actual.Close();if actual.Hash!=preview.Hash{t.Fatalf("exact approval manifest changed: %s != %s",actual.Hash,preview.Hash)};if len(actual.Changes)!=4{t.Fatalf("prior changes missing: %+v",actual.Changes)}
}

func TestZeroModePreservesExecutableAndDefaultsNewFile(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"script"),"old");if e:=os.Chmod(filepath.Join(root,"script"),0755);e!=nil{t.Fatal(e)};s:=snapshotTest(t,root)
 preview,e:=s.PreviewStage("script",[]byte("new"),0);if e!=nil{t.Fatal(e)};defer preview.Close();actual,e:=s.Stage("script",[]byte("new"),0);if e!=nil{t.Fatal(e)};defer actual.Close();if preview.Hash!=actual.Hash||actual.Changes[0].After.Mode!=0755{t.Fatal("zero mode stripped executable permission")}
 fresh,e:=s.Stage("new",[]byte("new"),0);if e!=nil{t.Fatal(e)};defer fresh.Close();info,e:=os.Stat(filepath.Join(s.Root,"new"));if e!=nil||info.Mode().Perm()!=0644{t.Fatalf("new default mode: %v %v",info,e)}
}
func TestAbsoluteInternalAndRootSymlinksRebasePrivately(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"target"),"data");if e:=os.Symlink(filepath.Join(root,"target"),filepath.Join(root,"absolute"));e!=nil{t.Fatal(e)};if e:=os.Symlink(".",filepath.Join(root,"root-link"));e!=nil{t.Fatal(e)};s:=snapshotTest(t,root);target,e:=os.Readlink(filepath.Join(s.Root,"absolute"));if e!=nil||target!="target"{t.Fatalf("absolute link was not private: %q %v",target,e)};m,e:=s.Changes(context.Background());if e!=nil{t.Fatal(e)};defer m.Close();if len(m.Changes)!=0{t.Fatal("rebased internal links appeared as host changes")}
}
func TestPublicationDescriptorSurvivesFilenameReplacement(t *testing.T){
 root:=t.TempDir();writeTestFile(t,filepath.Join(root,"file"),"approved");s:=snapshotTest(t,root);f,entry,e:=s.OpenRegular("file");if e!=nil{t.Fatal(e)};defer f.Close();if entry.Hash!=digest([]byte("approved")){t.Fatal("publication digest incorrect")};if e=os.Remove(filepath.Join(s.Root,"file"));e!=nil{t.Fatal(e)};writeTestFile(t,filepath.Join(s.Root,"file"),"replacement");v,e:=entryFor(f);if e!=nil||v.Hash!=entry.Hash{t.Fatalf("filename swap replaced approved descriptor: %v",e)}
}

func TestChangesetDeletesDirectoriesAndRestoresReadOnlyModes(t *testing.T){
 root:=t.TempDir();if e:=os.Mkdir(filepath.Join(root,"remove"),0755);e!=nil{t.Fatal(e)};writeTestFile(t,filepath.Join(root,"remove","file"),"old");s:=snapshotTest(t,root);if e:=os.RemoveAll(filepath.Join(s.Root,"remove"));e!=nil{t.Fatal(e)};if e:=os.Mkdir(filepath.Join(s.Root,"readonly"),0755);e!=nil{t.Fatal(e)};writeTestFile(t,filepath.Join(s.Root,"readonly","file"),"new");if e:=os.Chmod(filepath.Join(s.Root,"readonly"),0555);e!=nil{t.Fatal(e)}
 t.Cleanup(func(){_=os.Chmod(filepath.Join(root,"readonly"),0755)})
 m,e:=s.Changes(context.Background());if e!=nil{t.Fatal(e)};defer m.Close();records:=map[string]string{};if e=m.Apply(context.Background(),root,func()error{return nil},func(r ApplyRecord)error{records[r.Path]=r.State;return nil});e!=nil{t.Fatal(e)};if _,e=os.Stat(filepath.Join(root,"remove"));!os.IsNotExist(e){t.Fatal("deleted directory remained")};info,e:=os.Stat(filepath.Join(root,"readonly"));if e!=nil||info.Mode().Perm()!=0555{t.Fatalf("directory mode not restored: %v %v",info,e)};for _,state:=range records{if state!="applied"{t.Fatalf("unfinished directory journal: %v",records)}}
}
func TestStoreAliasCannotMaterializeInsideOriginal(t *testing.T){
 root:=t.TempDir();alias:=filepath.Join(t.TempDir(),"alias");if e:=os.Symlink(root,alias);e!=nil{t.Fatal(e)};s,e:=SnapshotWorkspace(context.Background(),root,filepath.Join(alias,"private-store"),Limits{});if s!=nil{s.Close()};if !errors.Is(e,ErrDenied){t.Fatalf("store alias accepted: %v",e)};if _,e=os.Stat(filepath.Join(root,"private-store"));!os.IsNotExist(e){t.Fatal("unsafe store was created before root validation")}
}
