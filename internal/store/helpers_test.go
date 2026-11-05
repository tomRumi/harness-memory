package store

import "context"

// afterWrite indexes files a test wrote by hand and tells the committer (what a Tx does for its own writes).
func (s *Store) afterWrite(ctx context.Context, rels ...string) error {
	for _, r := range rels {
		if err := s.DB.Upsert(s.Repo, r); err != nil {
			return err
		}
	}
	s.finish(ctx, "", rels...)
	return nil
}
