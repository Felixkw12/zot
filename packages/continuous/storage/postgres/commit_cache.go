package postgres

import "github.com/patriceckhart/zot/packages/continuous/storage"

const recentCommitBytes = 4 << 20
const recentCommitEntries = 128

type cachedCommit struct {
	commit storage.Commit
	bytes  int
}

// cacheCommitLocked retains only acknowledged commits for live watchers.
// Historical cursors and oversized commits still use checked database reads.
func (s *Store) cacheCommitLocked(c storage.Commit) {
	if s.state != stateOpen {
		return
	}
	size := len(c.Actor)
	for _, op := range c.Operations {
		size += len(op.Key) + len(op.Value)
	}
	if size > recentCommitBytes {
		s.recent, s.recentBytes = nil, 0
		return
	}
	if len(s.recent) > 0 && s.recent[len(s.recent)-1].commit.Revision+1 != c.Revision {
		s.recent, s.recentBytes = nil, 0
	}
	for len(s.recent) > 0 && (len(s.recent) >= recentCommitEntries || s.recentBytes+size > recentCommitBytes) {
		s.recentBytes -= s.recent[0].bytes
		// Clear removed payload references even if the backing array survives.
		s.recent[0] = cachedCommit{}
		s.recent = s.recent[1:]
	}
	s.recent = append(s.recent, cachedCommit{commit: cloneCommit(c), bytes: size})
	s.recentBytes += size
}

func (s *Store) cachedCommitsLocked(after, high uint64, limit int) ([]storage.Commit, bool) {
	if len(s.recent) == 0 {
		return nil, false
	}
	first := s.recent[0].commit.Revision
	end := after + min(uint64(limit), high-after)
	if after+1 < first || end > s.recent[len(s.recent)-1].commit.Revision {
		return nil, false
	}
	out := make([]storage.Commit, 0, end-after)
	for revision := after + 1; revision <= end; revision++ {
		c := cloneCommit(s.recent[revision-first].commit)
		if c.Operations == nil {
			c.Operations = []storage.Operation{}
		}
		out = append(out, c)
	}
	return out, true
}
