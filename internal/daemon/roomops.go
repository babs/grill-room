package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// RemoveRoom takes room off this computer (003b-AC6): it revokes the daemon token on the room
// server, ends the room's run and its sandboxes, and deletes its token, workspace cache and plugin
// option values. A server that does not answer keeps the token valid there: the removal fails,
// unless force, which removes the room here only.
func (d *Daemon) RemoveRoom(ctx context.Context, link string, force bool) error {
	room, err := ParseRoomURL(link)
	if err != nil {
		return err
	}
	key := room.String()
	d.mu.Lock()
	if d.removing[key] {
		d.mu.Unlock()
		return errRemoving
	}
	d.removing[key] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.removing, key)
		d.mu.Unlock()
	}()
	tok, err := d.o.Tokens.Get(room)
	if err != nil {
		return err
	}
	if tok != "" {
		if err := d.revoke(ctx, room, tok); err != nil && !force {
			return err
		}
	}
	d.mu.Lock()
	r := d.rooms[key]
	delete(d.rooms, key)
	delete(d.ready, key)
	if d.cur == r {
		d.cur = nil
	}
	// A pairing waiting for approval keeps polling, dropped from the list: an approval that comes
	// later is revoked by its loop, not left as a computer the room server lists. It ends when the
	// request is answered or expires.
	pairing := r != nil && (r.st.Pairing == "requesting" || r.st.Pairing == "pending")
	d.mu.Unlock()
	if r != nil && !pairing {
		r.cancel()
	}
	// A pairing approved before the run was dropped has stored its token since the read above;
	// none can land now (roomRun.loop stores only for a live run).
	if tok == "" {
		if late, gerr := d.o.Tokens.Get(room); gerr == nil && late != "" {
			if rerr := d.revoke(ctx, room, late); rerr != nil {
				d.o.Log.Warn("pairing_revoke_failed", zap.String("room", key), zap.Error(rerr))
			}
		}
	}
	err = errors.Join(d.o.Tokens.Set(room, ""), d.o.PluginOptions.Keep(key, nil), d.forgetRoomEgress(ctx, key))
	d.update(func(*Status) {})
	d.o.Log.Info("room_removed", zap.String("room", key), zap.Bool("revoked_here_only", force))
	// The reply goes now, within the supervisor's request timeout: removing the sandboxes of a
	// running answer takes up to 30 s each. The loop removes them as it ends, then the cache goes.
	d.loops.Go(func() {
		if r != nil {
			<-r.done
		}
		if d.cache != nil {
			if err := d.cache.forget(room); err != nil {
				d.o.Log.Error("workspace_cache_delete_failed", zap.Error(err))
			}
		}
	})
	return err
}

// revoke asks the room server to delete this daemon. A token the room no longer knows (401) is as
// good as revoked; a 404 is a room server from before 003b, which kept it.
func (d *Daemon) revoke(ctx context.Context, room Room, tok string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := d.roomRequest(ctx, http.MethodDelete, room.Base+"/api/rooms/"+room.ID+"/daemon", tok, nil)
	if err != nil {
		return fmt.Errorf("the room server did not answer, so it still lists this computer: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusUnauthorized:
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return errors.New("this room server is too old to forget this computer: update it, or remove the room here only")
	}
	return fmt.Errorf("the room server refused to remove this computer: %w", apiError(resp))
}
