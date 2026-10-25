//go:build linux

package sandbox

import (
 "fmt"
 "io"
 "os"
 "path/filepath"
 "strings"
)

// CleanupStore is called once during daemon startup before accepting workers.
// Durable changeset directories are retained by opaque ID; private application
// state is never treated as an orphan merely because it is currently idle.
func CleanupStore(store string,retained map[string]bool)error{
 storeMu.Lock();defer storeMu.Unlock();if ActiveWorkers()!=0{return fmt.Errorf("active confined workers prevent transient store cleanup")}
 dir,e:=os.Open(store);if os.IsNotExist(e){return nil};if e!=nil{return e};defer dir.Close()
 for {entries,e:=dir.ReadDir(128);for _,d:=range entries{name:=d.Name();remove:=strings.HasPrefix(name,"snapshot-")||strings.HasPrefix(name,"manifest-")||strings.HasPrefix(name,"auth-");if strings.HasPrefix(name,"changeset-"){remove=!retained[strings.TrimPrefix(name,"changeset-")]};if remove{if e:=removePrivate(filepath.Join(store,name));e!=nil{return e}}};if e!=nil{if e==io.EOF{return nil};return e}}
}
