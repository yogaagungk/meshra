//go:build linux

package sandbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// PrivateState is a capability created by the daemon, not a model-selectable path.
type PrivateState struct {
	path  string
	store string
}

func OpenPrivateState(store, id string) (*PrivateState, error) {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\.\x00") {
		return nil, ErrDenied
	}
	var err error
	store, err = filepath.Abs(store)
	if err != nil {
		return nil, err
	}
	if e := os.MkdirAll(store, 0700); e != nil {
		return nil, e
	}
	st, e := os.Lstat(store)
	if e != nil {
		return nil, e
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 {
		return nil, ErrDenied
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, ErrDenied
	}
	path := filepath.Join(store, "state-"+id)
	if e = os.Mkdir(path, 0700); e != nil && !os.IsExist(e) {
		return nil, e
	}
	st, e = os.Lstat(path)
	if e != nil {
		return nil, e
	}
	stat, ok = st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || st.Mode().Perm() != 0700 || int(stat.Uid) != os.Geteuid() {
		return nil, ErrDenied
	}
	return &PrivateState{path: path, store: store}, nil
}

// Remove discards daemon-owned disposable state after its worker has drained.
func (s *PrivateState) Remove() error {
	if s == nil || s.path == "" || filepath.Dir(s.path) != s.store {
		return ErrDenied
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	return removePrivate(s.path)
}
func (p *Prepared) authMounts(q Request) ([]string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if len(q.AuthFiles) > 16 {
		return nil, ErrLimit
	}
	if len(q.AuthFiles) != 0 && q.Kind != ACP {
		return nil, ErrDenied
	}
	args := []string{}
	for _, a := range q.AuthFiles {
		if !strings.HasPrefix(a.Target, "/home/worker/") || !validPath(strings.TrimPrefix(a.Target, "/home/worker/")) || !validHash(a.SHA256) {
			return nil, ErrDenied
		}
		source, e := os.OpenFile(a.Source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			return nil, e
		}
		st, e := source.Stat()
		if e != nil {
			source.Close()
			return nil, e
		}
		if !st.Mode().IsRegular() || st.Size() > 8<<20 {
			source.Close()
			return nil, ErrDenied
		}
		v, e := entryForLimited(source, 8<<20)
		if e != nil {
			source.Close()
			return nil, e
		}
		if v.Hash != a.SHA256 {
			source.Close()
			return nil, fmt.Errorf("%w: selected authentication file changed", ErrUnavailable)
		}
		if e = checkStore(q.Workspace.store, st.Size()+8192, q.Workspace.limits); e != nil {
			source.Close()
			return nil, e
		}
		dir, e := os.MkdirTemp(q.Workspace.store, "auth-")
		if e != nil {
			source.Close()
			return nil, e
		}
		p.cleanup = append(p.cleanup, dir)
		target := filepath.Join(dir, "file")
		out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if e != nil {
			source.Close()
			return nil, e
		}
		_, e = io.Copy(out, io.LimitReader(source, 8<<20+1))
		source.Close()
		out.Close()
		if e != nil {
			return nil, e
		}
		f, e := os.Open(target)
		if e != nil {
			return nil, e
		}
		v, e = entryFor(f)
		f.Close()
		if e != nil {
			return nil, e
		}
		if v.Hash != a.SHA256 {
			return nil, ErrConflict
		}
		args = append(args, "--ro-bind", target, a.Target)
	}
	return args, nil
}
