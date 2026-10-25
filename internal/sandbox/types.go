// Package sandbox implements fail-closed Linux workers and private workspace changes.
package sandbox

import (
 "context"
 "crypto/rand"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "os"
 "path/filepath"
 "strings"
 "time"
 "unicode/utf8"
)

var ErrUnavailable = errors.New("confined execution unavailable")
var ErrConflict = errors.New("workspace changed")
var ErrLimit = errors.New("sandbox size limit exceeded")
var ErrDenied = errors.New("sandbox access denied")

type Limits struct {
 SnapshotBytes int64 `json:"snapshotBytes"`
 SnapshotFiles int `json:"snapshotFiles"`
 StoreBytes int64 `json:"storeBytes"`
 CgroupRoot string `json:"cgroupRoot,omitempty"`
}
func (l Limits) defaults() Limits { if l.SnapshotBytes == 0 { l.SnapshotBytes = 512<<20 }; if l.SnapshotFiles == 0 { l.SnapshotFiles = 20000 }; if l.StoreBytes == 0 { l.StoreBytes = 2<<30 }; return l }
func (l Limits) validate() error { if l.SnapshotBytes < 1 || l.SnapshotBytes > 4<<30 || l.SnapshotFiles < 1 || l.SnapshotFiles > 100000 || l.StoreBytes < l.SnapshotBytes || l.StoreBytes > 64<<30 { return fmt.Errorf("invalid snapshot/store limits") }; return nil }

type Asset struct { Path string `json:"path"`; SHA256 string `json:"sha256,omitempty"`; System bool `json:"system"` }
type Config struct {
 Executables map[string]string `json:"executables"`
 RuntimePaths []string `json:"runtimePaths"`
 Limits Limits `json:"limits"`
 Manifest []Asset `json:"manifest"`
}
type Status struct { State string `json:"state"`; Prerequisites []string `json:"prerequisites"` }
type Kind string
const ( Command Kind = "command"; Browser Kind = "browser"; Voice Kind = "voice"; ACP Kind = "acp" )
type SocketMount struct { Source string; Target string; Purpose string }
type AuthFile struct { Source string; Target string; SHA256 string }
type Request struct {
 Workspace *Snapshot
 Argv []string
 Kind Kind
 // Zero selects command=60s/voice=180s. ACP/browser use the owning lifecycle
 // context; their turn deadlines and browser idle lease are enforced by callers.
 Timeout time.Duration
 // Env contains explicitly selected values, never the inherited daemon environment.
 Env map[string]string
 ReadOnly bool
 SocketMounts []SocketMount
 AuthFiles []AuthFile
 State *PrivateState
 BeforeStart func() error
}
type Result struct { ExitCode int `json:"exitCode"`; Stdout string `json:"stdout"`; Stderr string `json:"stderr"`; Truncated bool `json:"truncated"`; Changes *Manifest `json:"changes,omitempty"` }
type Entry struct { Kind string `json:"kind"`; Hash string `json:"hash,omitempty"`; Mode uint32 `json:"mode"`; Link string `json:"link,omitempty"` }
type Change struct { Path string `json:"path"`; Before *Entry `json:"before,omitempty"`; After *Entry `json:"after,omitempty"`; Blob string `json:"-"` }
type Manifest struct { Hash string `json:"hash"`; Changes []Change `json:"changes"`; Preview string `json:"preview"`; PreviewTruncated bool `json:"previewTruncated"`; directory string; store string; limits Limits }
type ApplyRecord struct { Path string; State string; OriginalHash string; ResultHash string; Error string }
type Journal func(ApplyRecord) error
// Claim is called after preflight and immediately before the first filesystem mutation.
// Each subsequent mutation calls it again. It must revalidate the active approval/session.
type Claim func() error
func digest(v []byte) string { h:=sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func (m *Manifest) seal() { b,_:=json.Marshal(m.Changes); m.Hash=digest(b) }
func validPath(p string) bool { if p=="" || p=="." || !utf8.ValidString(p) || filepath.IsAbs(p) || filepath.Clean(p)!=p || strings.ContainsAny(p,"\\\x00") || strings.IndexFunc(p,func(r rune)bool{return r<32||r==127})>=0 || len(p)>4096 { return false }; for _,s:=range strings.Split(p,"/") { if s==".." || strings.EqualFold(s,".git") { return false } }; return true }
func protectedName(p string) bool { return strings.EqualFold(p,".git") }
func missing(err error) bool { return errors.Is(err,os.ErrNotExist) }
func contextError(ctx context.Context) error { select { case <-ctx.Done(): return ctx.Err(); default:return nil } }
func temporaryName()(string,error){var b [16]byte;if _,e:=rand.Read(b[:]);e!=nil{return "",e};return ".meshra-"+hex.EncodeToString(b[:]),nil}
