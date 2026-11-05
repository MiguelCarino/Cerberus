package session

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// maxWallIDs bounds one multiplexed stream.
const maxWallIDs = 16

// WallChunk is a piece of one session's output on a multiplexed stream.
type WallChunk struct {
	ID   string
	Data []byte
}

// AttachMany attaches to several sessions at once for a wall of terminals:
// each session's last backlogKiB KiB of output (at least 1 KiB) and one
// channel carrying everyone's new output, tagged by session id.
//
// The channel closes when any of the sessions' streams ends (the reader fell
// behind, or a session was accepted or discarded) or after detach; the
// client then reattaches for fresh backlogs. Call detach when done. Ids are
// deduplicated; at most 16; an unknown id fails the whole call.
func (m *Manager) AttachMany(ids []string, backlogKiB int) (map[string][]byte, <-chan WallChunk, func(), error) {
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !slices.Contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return nil, nil, nil, errors.New("no sessions to attach")
	}
	if len(uniq) > maxWallIDs {
		return nil, nil, nil, fmt.Errorf("at most %d sessions per wall stream", maxWallIDs)
	}
	m.mu.Lock()
	outs := make([]*output, len(uniq))
	for i, id := range uniq {
		s, err := m.lookup(id)
		if err != nil {
			m.mu.Unlock()
			return nil, nil, nil, fmt.Errorf("%s: %w", id, err)
		}
		outs[i] = s.out
	}
	m.mu.Unlock()

	limit := max(backlogKiB, 1) << 10
	backlogs := make(map[string][]byte, len(uniq))
	merged := make(chan WallChunk, subBuffer)
	stop := make(chan struct{})
	var stopOnce sync.Once
	end := func() { stopOnce.Do(func() { close(stop) }) }
	var detaches []func()
	var wg sync.WaitGroup
	for i, id := range uniq {
		backlog, ch, detach := outs[i].attach()
		if len(backlog) > limit {
			backlog = backlog[len(backlog)-limit:]
		}
		backlogs[id] = backlog
		detaches = append(detaches, detach)
		wg.Go(func() {
			defer end() // one stream ended: end them all, so the client reattaches
			for {
				select {
				case data, ok := <-ch:
					if !ok {
						return
					}
					select {
					case merged <- WallChunk{ID: id, Data: data}:
					case <-stop:
						return
					}
				case <-stop:
					return
				}
			}
		})
	}
	go func() {
		wg.Wait()
		close(merged)
	}()
	var detachOnce sync.Once
	detachAll := func() {
		detachOnce.Do(func() {
			end()
			for _, d := range detaches {
				d()
			}
		})
	}
	// A forwarder that ends on its own must also release the others'
	// subscriptions, or their outputs would keep feeding a dead channel
	// until the client calls detach.
	go func() {
		<-stop
		detachAll()
	}()
	return backlogs, merged, detachAll, nil
}
