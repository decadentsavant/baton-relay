package main

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"time"
)

// A baton is a wave that remembers. It carries three facts and no fourth:
// when it was born, how many times a human has deliberately passed it, and how
// many distinct countries it has been through.
//
// What it pointedly does not carry is an *ordered* trail. A sequence of
// countries with timestamps is a movement log, and in a community where a given
// country might hold a dozen users, a log like that says something about a
// person. A count says something only about the baton. The set below exists so
// distinct countries can be counted at all; it is unordered, untimed, and never
// appears in public responses - the API exposes its size and nothing else.
// The set is persisted with the baton so the count survives a restart.
type baton struct {
	ID        string          `json:"id"`
	Born      time.Time       `json:"born"`
	Hops      int64           `json:"hops"`
	Countries map[string]bool `json:"countries"`

	// Who is holding it right now. Lowercase, so encoding/json will not write
	// it to disk: holder identity is intentionally absent from persisted state.
	// On load every baton is an orphan and gets re-homed.
	holder string
}

func newBatonID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// How many batons should be in circulation for a given crowd. Too many and a
// baton is just a wave with a number on it; too few and almost nobody ever sees
// one. One per 25 connected clients keeps them rare enough to be a small event.
func batonTarget(online int) int {
	if online < 2 {
		return 0
	}
	target := online / 25
	if target < 1 {
		target = 1
	}
	if target > 64 {
		target = 64
	}
	return target
}

// --- the calls below all assume r.mu is already held ---

func newBaton(now time.Time) *baton {
	id, err := newBatonID()
	if err != nil {
		return nil
	}
	return &baton{ID: id, Born: now.UTC(), Countries: make(map[string]bool)}
}

func (b *baton) clone() *baton {
	copy := *b
	copy.Countries = make(map[string]bool, len(b.Countries))
	for cc, present := range b.Countries {
		copy.Countries[cc] = present
	}
	return &copy
}

func (r *relay) batonHeldByLocked(clientID string) *baton {
	for _, b := range r.batons {
		if b.holder == clientID {
			return b
		}
	}
	return nil
}

func (r *relay) batonFrameLocked(b *baton) map[string]any {
	if b == nil {
		return nil
	}
	return map[string]any{
		"id":        b.ID,
		"born":      b.Born.UTC().Format(time.RFC3339),
		"hops":      b.Hops,
		"countries": len(b.Countries),
	}
}

// --- re-homing ---

// A baton survives a holder disconnect. Clients can sleep while the one-hour
// cooldown is active, so the baton is orphaned and handed to a connected person
// rather than discarded.
//
// Re-homing is plumbing and deliberately does NOT count as a hop. A hop is a
// person deciding to pass something on; being handed a baton by the scheduler
// is not that.
func (r *relay) rehomeOrphans() {
	r.mu.Lock()
	defer r.mu.Unlock()
	free := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		if r.batonHeldByLocked(c.id) == nil {
			free = append(free, c)
		}
	}
	for _, b := range r.batons {
		if _, live := r.clients[b.holder]; live {
			continue
		}
		for len(free) > 0 {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(free))))
			if err != nil {
				return
			}
			i := int(n.Int64())
			next := free[i]
			free[i] = free[len(free)-1]
			free = free[:len(free)-1]
			if next.send(encode(frame{"type": "baton", "baton": r.batonFrameLocked(b)})) {
				b.holder = next.id
				break
			}
		}
	}
}
