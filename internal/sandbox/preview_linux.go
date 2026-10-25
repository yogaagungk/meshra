//go:build linux

package sandbox

import (
 "context"
 "fmt"
 "os"
 "path/filepath"
 "sort"
 "strings"
)

// PreviewStage seals the complete prospective baseline-to-working manifest,
// without mutating the private working tree. Existing approved changes remain.
// Mode zero preserves existing permission bits, or uses 0644 for a new file.
func(s *Snapshot)PreviewStage(path string,data []byte,mode os.FileMode)(m *Manifest,err error){
 if !validPath(path)||len(data)>1<<20||mode&^0777!=0{return nil,fmt.Errorf("invalid bounded regular-file replacement")}
 s.mu.Lock();defer s.mu.Unlock();storeMu.Lock();defer storeMu.Unlock();if s.closed{return nil,ErrDenied}
 root,_,e:=openRoot(s.rootPath);if e!=nil{return nil,e};defer root.Close()
 existing,e:=currentEntry(root,path);if e!=nil{return nil,e};if existing!=nil&&existing.Kind!="file"{return nil,ErrDenied}
 if mode==0{mode=0644;if existing!=nil{mode=os.FileMode(existing.Mode)}}
 if e=checkStore(s.store,int64(len(data))+4096,s.limits);e!=nil{return nil,e}
 m,e=s.changesLocked(context.Background());if e!=nil{return nil,e};created:=m;defer func(){if err!=nil{created.Close()}}()
 if e=checkStore(s.store,int64(len(data))+4096,s.limits);e!=nil{return nil,e}
 indexed:=map[string]Change{};for _,c:=range m.Changes{indexed[c.Path]=c}
 after:=Entry{Kind:"file",Hash:digest(data),Mode:uint32(mode.Perm())};change:=Change{Path:path,After:&after};if old,ok:=s.original[path];ok{if old.Kind!="file"{return nil,ErrDenied};v:=old;change.Before=&v}
 if sameEntry(change.Before,change.After){delete(indexed,path)}else{change.Blob=filepath.Join(m.directory,after.Hash);if _,e=os.Stat(change.Blob);os.IsNotExist(e){if e=os.WriteFile(change.Blob,data,0400);e!=nil{return nil,e}}else if e!=nil{return nil,e};indexed[path]=change}
 parent:=filepath.Dir(path)
 for parent!="."{v,e:=currentEntry(root,parent);if e!=nil{return nil,e};if v!=nil{if v.Kind!="directory"{return nil,ErrDenied}}else{v:=Entry{Kind:"directory",Mode:0755};c:=Change{Path:parent,After:&v};if old,ok:=s.original[parent];ok{x:=old;c.Before=&x};indexed[parent]=c};parent=filepath.Dir(parent)}
 m.Changes=m.Changes[:0];for _,c:=range indexed{m.Changes=append(m.Changes,c)};sort.Slice(m.Changes,func(i,j int)bool{return m.Changes[i].Path<m.Changes[j].Path});m.renderPreview(s.originalText);m.seal();return m,nil
}
func(m *Manifest)renderPreview(original map[string]string){
 var b strings.Builder;m.PreviewTruncated=false
 for _,c:=range m.Changes{if b.Len()>=64<<10{m.PreviewTruncated=true;break};before,after:="absent","absent";if c.Before!=nil{before=c.Before.Hash;if c.Before.Kind=="directory"{before=fmt.Sprintf("directory mode %04o",c.Before.Mode)}};if c.After!=nil{after=c.After.Hash;if c.After.Kind=="directory"{after=fmt.Sprintf("directory mode %04o",c.After.Mode)}};fmt.Fprintf(&b,"--- %s (%s)\n+++ %s (%s)\n",c.Path,before,c.Path,after)
  if c.Before!=nil&&c.Before.Kind=="file"{if text,ok:=original[c.Path];ok{b.WriteString("BEFORE\n");b.WriteString(text);b.WriteByte('\n')}else{b.WriteString("BEFORE content omitted; exact SHA-256 above\n");m.PreviewTruncated=true}}
  if c.After!=nil&&c.After.Kind=="file"{b.WriteString("AFTER\n");f,e:=os.Open(c.Blob);if e!=nil{m.PreviewTruncated=true;continue};var buf [4096]byte;for b.Len()<64<<10{n,e:=f.Read(buf[:min(len(buf),64<<10-b.Len()+1)]);b.Write(buf[:n]);if e!=nil{break}};f.Close();b.WriteByte('\n')}
 }
 text:=b.String();if len(text)>64<<10{text=text[:64<<10];m.PreviewTruncated=true};m.Preview=text
}
