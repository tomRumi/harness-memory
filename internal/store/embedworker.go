package store

import (
	"context"
	"fmt"
	"time"
)

// finish tells the committer about files whose index entries are already up to date (msg: the commit message, empty
// for the default) and asks for embedding. It does no slow work, so it is safe under the write lock.
func (s *Store) finish(ctx context.Context, msg string, rels ...string) {
	if s.Commit != nil {
		s.Commit.NotifyMsg(msg, rels...)
	}
	s.kickEmbed()
}

// kickEmbed starts the embedding worker, or tells the running one to make another pass. There is one worker at a
// time. A pass has EmbedTimeout; OnWrite is called after each pass (and at once when there is no embedder).
func (s *Store) kickEmbed() {
	emb := s.Emb
	if emb == nil {
		if s.OnWrite != nil {
			s.OnWrite()
		}
		return
	}
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	if s.closed {
		return
	}
	s.embedWant, s.embedWith = true, emb
	if s.embedRun {
		return
	}
	s.embedRun = true
	go s.embedLoop()
}

func (s *Store) embedLoop() {
	for {
		s.embedMu.Lock()
		if !s.embedWant {
			s.embedRun = false
			s.embedCond.Broadcast()
			s.embedMu.Unlock()
			return
		}
		s.embedWant = false
		emb := s.embedWith
		s.embedMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), EmbedTimeout)
		_, err := s.DB.EmbedPending(ctx, emb, nil)
		if err == nil && ctx.Err() != nil {
			err = fmt.Errorf("embedding did not finish in %s: %w", EmbedTimeout, ctx.Err()) // EmbedPending stops quietly at the deadline
		}
		cancel()
		s.embedMu.Lock()
		s.embedErr = err
		if err != nil {
			s.embedWant = false // a failing embedder is not asked again by every write; the catch-up loop retries
		}
		s.embedMu.Unlock()
		if s.OnWrite != nil {
			s.OnWrite()
		}
	}
}

// WaitEmbedMax is the longest WaitEmbed (and so Close) waits for the embedding worker, however many writes arrive.
const WaitEmbedMax = 5 * time.Second

// WaitEmbed returns when no embedding is running, or after WaitEmbedMax at the latest. Tests use it; a write never
// waits for it.
func (s *Store) WaitEmbed() {
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	if !s.embedRun {
		return
	}
	t := time.AfterFunc(WaitEmbedMax, func() {
		s.embedMu.Lock()
		s.embedCond.Broadcast()
		s.embedMu.Unlock()
	})
	defer t.Stop()
	deadline := time.Now().Add(WaitEmbedMax)
	for s.embedRun && time.Now().Before(deadline) {
		s.embedCond.Wait()
	}
}

// EmbedError is the error of the last embedding attempt after a write; nil when it worked or none was made.
func (s *Store) EmbedError() error {
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	return s.embedErr
}
