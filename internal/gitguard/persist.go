package gitguard

import "encoding/json"

// persisted is the on-disk shape of a Baseline. others is unexported (it is
// guarded by mu and holds live probe state), so a hook that must remember
// every repository a session has touched round-trips through this.
type persisted struct {
	Root      string                `json:"root"`
	Tracked   []string              `json:"tracked,omitempty"`
	Untracked []string              `json:"untracked,omitempty"`
	Others    map[string]*persisted `json:"others,omitempty"`
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
	p := &persisted{Root: b.Root, Tracked: b.Tracked, Untracked: b.Untracked}
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
	b := &Baseline{Root: p.Root, Tracked: p.Tracked, Untracked: p.Untracked}
	if len(p.Others) > 0 {
		b.others = map[string]*Baseline{}
		for k, o := range p.Others {
			b.others[k] = fromPersisted(o)
		}
	}
	return b
}
