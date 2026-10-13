package gitguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// persisted is the on-disk shape of a Baseline. others is unexported (it is
// guarded by mu and holds live probe state), so a hook that must remember
// every repository a session has touched round-trips through this.
type persisted struct {
	Root      string                `json:"root"`
	Tracked   []string              `json:"tracked,omitempty"`
	Untracked []string              `json:"untracked,omitempty"`
	Work      map[string]string     `json:"work,omitempty"`
	Index     map[string]string     `json:"index,omitempty"`
	Others    map[string]*persisted `json:"others,omitempty"`
	Identity  *persistedIdentity    `json:"identity,omitempty"`
}

// persistedIdentity is a gitIdentity on disk. Without it a reloaded baseline
// of another work tree never passes Baseline.other's identity check: it is
// dropped at the next touch and re-captured, which records the session's own
// edits in that tree as pre-existing (WI-1300).
type persistedIdentity struct {
	DotGit   persistedEntry    `json:"dot_git"`
	Gitdir   string            `json:"gitdir,omitempty"`
	Configs  []persistedConfig `json:"configs,omitempty"`
	NoConfig bool              `json:"no_config,omitempty"`
}

type persistedEntry struct {
	Exists bool   `json:"exists"`
	IsDir  bool   `json:"is_dir"`
	Dev    uint64 `json:"dev"`
	Ino    uint64 `json:"ino"`
	Known  bool   `json:"known"`
}

type persistedConfig struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
	Nsec int64  `json:"nsec"`
}

// MarshalBaseline encodes b, including the baselines of other work trees the
// run has already touched. Probe caches are not part of the state.
func MarshalBaseline(b *Baseline) ([]byte, error) {
	return json.MarshalIndent(toPersisted(b), "", "  ")
}

// UnmarshalBaseline is the inverse of MarshalBaseline.
func UnmarshalBaseline(data []byte) (*Baseline, error) {
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return fromPersisted(&p), nil
}

func toPersisted(b *Baseline) *persisted {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := &persisted{Root: b.Root, Tracked: b.Tracked, Untracked: b.Untracked, Work: b.Work, Index: b.Index}
	if b.id.dotGit.exists {
		id := &persistedIdentity{
			DotGit:   persistedEntry{b.id.dotGit.exists, b.id.dotGit.isDir, b.id.dotGit.dev, b.id.dotGit.ino, b.id.dotGit.known},
			Gitdir:   b.id.gitdir,
			NoConfig: b.id.noConfig,
		}
		for _, c := range b.id.configs {
			id.Configs = append(id.Configs, persistedConfig{c.path, c.dev, c.ino, c.nsec})
		}
		p.Identity = id
	}
	if len(b.others) > 0 {
		p.Others = map[string]*persisted{}
		for k, o := range b.others {
			p.Others[k] = toPersisted(o)
		}
	}
	return p
}

func fromPersisted(p *persisted) *Baseline {
	if p == nil {
		return nil
	}
	b := &Baseline{Root: p.Root, Tracked: p.Tracked, Untracked: p.Untracked, Work: p.Work, Index: p.Index}
	if id := p.Identity; id != nil {
		b.id = gitIdentity{
			dotGit:   entryStamp{id.DotGit.Exists, id.DotGit.IsDir, id.DotGit.Dev, id.DotGit.Ino, id.DotGit.Known},
			gitdir:   id.Gitdir,
			noConfig: id.NoConfig,
		}
		for _, c := range id.Configs {
			b.id.configs = append(b.id.configs, configStamp{c.Path, c.Dev, c.Ino, c.Nsec})
		}
	}
	if len(p.Others) > 0 {
		b.others = map[string]*Baseline{}
		for k, o := range p.Others {
			b.others[k] = fromPersisted(o)
		}
	}
	return b
}

// Resume returns the baseline a session recorded at path, or, when there is
// none, a fresh capture of the repository containing cwd. A resumed session —
// after an engine restart, or a hibernate and wake — must judge commands by
// the state it first saw, not by a capture taken now: a capture now records
// the files it has written since as the user's, and refuses to commit them.
//
// Reusing the file is the permissive direction only for the session's own
// writes. Every path dirty in the original capture stays protected, and a
// reloaded baseline of another work tree is still re-checked against its
// recorded identity before it is used. A file that exists but cannot be read
// is an error, not a reason to capture afresh. ErrNotRepo is returned, as by
// Capture, when there is no file and cwd is not in a repository.
func Resume(path, cwd string) (*Baseline, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			b, err := UnmarshalBaseline(data)
			if err != nil {
				return nil, fmt.Errorf("baseline %s is not readable: %w", path, err)
			}
			if b.Root == "" {
				return nil, fmt.Errorf("baseline %s names no repository", path)
			}
			return b, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading baseline: %w", err)
		}
	}
	return Capture(cwd)
}

// Save writes b to path atomically, readable only by the user.
func (b *Baseline) Save(path string) error {
	data, err := MarshalBaseline(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PersistentGuard is Guard, saving b to path after every call so that a work
// tree first touched by that call is on record before the call runs, and is
// still on record if the process stops and the session is resumed. Saves are
// best effort: a failed write leaves the in-process guard intact.
func (b *Baseline) PersistentGuard(path string) func(tool string, input []byte, cwd string) string {
	if b == nil {
		return nil
	}
	if path == "" {
		return b.Guard()
	}
	var mu sync.Mutex
	_ = b.Save(path)
	return func(tool string, input []byte, cwd string) string {
		msg := b.CheckTool(tool, input, cwd)
		mu.Lock()
		_ = b.Save(path)
		mu.Unlock()
		return msg
	}
}
