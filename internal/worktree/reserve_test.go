package worktree

import (
	"sync"
	"testing"
)

// Two writers of the same type started together must not share a checkout.
func TestReserveIsUniqueUnderContention(t *testing.T) {
	root := t.TempDir()
	const n = 16
	dirs := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			d, err := reserve(root, "general-purpose")
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			dirs[i] = d
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" || seen[d] {
			t.Fatalf("dirs = %v, want %d distinct", dirs, n)
		}
		seen[d] = true
	}
}
