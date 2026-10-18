package continuous

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Rename updates display metadata without changing identity, history, or policy.
// expected is the conversation revision, not the global store revision.
// An empty title clears the name. Existing records without titles remain valid.
func (r *Runtime) Rename(ctx context.Context, id string, expected uint64, title string) (Conversation, error) {
	title = strings.TrimSpace(title)
	if !utf8.ValidString(title) || utf8.RuneCountInString(title) > 120 || strings.ContainsFunc(title, unicode.IsControl) {
		return Conversation{}, fmt.Errorf("title must be at most 120 characters without control characters")
	}
	for {
		if err := ctx.Err(); err != nil {
			return Conversation{}, err
		}
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		c, err := conversation(snap, id)
		if err != nil {
			return Conversation{}, err
		}
		if c.Revision != expected {
			return c, storage.ErrConflict
		}
		if c.Title == title {
			return c, nil
		}
		c.Title, c.Revision = title, snap.Revision()+1
		err = r.commit(ctx, snap, "conversation.rename", record("conversation/"+id, c))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}
