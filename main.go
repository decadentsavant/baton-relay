// Baton relay: routes one bit between strangers.
//
// This service uses only the Go standard library and keeps the protocol small
// enough to audit alongside the public documentation:
//
//  1. No message content exists. A wave carries a two-letter country code and
//     nothing else - there is no field an insult could travel in.
//  2. No IP address is ever stored. Country is resolved at request time, from
//     a trusted edge header or an in-memory range list, and the address itself
//     is hashed with a per-process salt purely for abuse accounting, so nothing
//     here survives a restart or correlates across one.
//  3. No social graph is built. The relay knows who is connected right now and
//     forgets it the moment they disconnect. It cannot tell you who waved at
//     whom, because it never writes that down.
//
// Durable state contains the total and baton aggregates, never holders or a wave history.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// Scarcity is the product. A wave you can send every second is noise, and
	// the received wave only feels like something because the sender spent
	// their whole hour on it.
	defaultCooldown = time.Hour

	// A wave that found nobody online affected nobody, so it only costs this
	// much. Early on, the crowd will often be one person, and locking them out
	// for an hour because they clicked into an empty room is not scarcity, it is
	// just unkind.
	defaultEmptyCooldown = time.Minute

	// One shared address is not one person: households, offices and carrier
	// NAT put many people behind one IP. The address budget exists only to stop
	// identity rotation from turning the cooldown into nothing, so it is a small
	// multiple of one, not one.
	defaultAddressWaves = 4

	// Open streams count toward "online" and toward who receives waves, so an
	// address gets a handful, not an unlimited supply.
	defaultStreamsPerAddress = 8

	// The stream is mostly silent, so a heartbeat is what lets a client tell
	// "nobody is waving" apart from "my socket died twenty minutes ago".
	pingInterval  = 30 * time.Second
	statsInterval = 15 * time.Second

	// A dropped connection should not instantly strip someone of a baton they
	// are about to come back to, so orphans are only re-homed on this tick.
	rehomeInterval = 90 * time.Second

	// Per-client outbound buffer. A client that cannot keep up with this is
	// wedged, and gets dropped rather than backing up the sender's request.
	sendBuffer         = 8
	streamWriteTimeout = 10 * time.Second
)

type frame map[string]any

type client struct {
	index int
	id    string
	addr  string
	out   chan []byte

	// Closed to retire this connection. `out` is deliberately never closed:
	// broadcastStats snapshots the client set under the lock and then sends
	// after releasing it, so a close racing that send would panic the process.
	done      chan struct{}
	closeOnce sync.Once
}

func (c *client) retire() {
	c.closeOnce.Do(func() { close(c.done) })
}

type relay struct {
	saveMu    sync.Mutex
	mu        sync.Mutex
	clients   map[string]*client
	connected []*client
	streams   map[string]int // open streams per address key
	idReady   map[string]time.Time
	addrWaves map[string][]time.Time
	batons    map[string]*baton

	trustedProxies []netip.Prefix
	salt           []byte
	geo            *countryDB
	cooldown       time.Duration
	emptyCooldown  time.Duration
	addrBudget     int
	streamCap      int
	minClient      string

	total     int64
	statePath string
	dirty     bool
}

func newRelay(statePath string, cooldown time.Duration) (*relay, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	r := &relay{
		clients:       make(map[string]*client),
		streams:       make(map[string]int),
		idReady:       make(map[string]time.Time),
		addrWaves:     make(map[string][]time.Time),
		batons:        make(map[string]*baton),
		salt:          salt,
		cooldown:      cooldown,
		emptyCooldown: min(cooldown, defaultEmptyCooldown),
		addrBudget:    defaultAddressWaves,
		streamCap:     defaultStreamsPerAddress,
		statePath:     statePath,
	}
	r.loadState()
	return r, nil
}

// ---------------------------------------------------------------- state

type persisted struct {
	Total  int64    `json:"total"`
	Batons []*baton `json:"batons"`
}

func (r *relay) loadState() {
	if r.statePath == "" {
		return
	}
	raw, err := os.ReadFile(r.statePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("state: read %s: %v", r.statePath, err)
		}
		return
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		log.Printf("state: parse %s: %v", r.statePath, err)
		return
	}
	r.total = p.Total
	for _, b := range p.Batons {
		if b == nil || b.ID == "" {
			continue
		}
		if b.Countries == nil {
			b.Countries = make(map[string]bool)
		}
		// holder is not persisted by design, so every baton comes back an
		// orphan and the re-home tick hands it to whoever is around.
		b.holder = ""
		r.batons[b.ID] = b
	}
	log.Printf("state: resumed at %d waves, %d batons", p.Total, len(p.Batons))
}

// Save an immutable snapshot. Writers can continue while disk I/O runs; any
// newer mutation leaves dirty set for the next save. Only one save owns .tmp.
func (r *relay) saveState() {
	if r.statePath == "" {
		return
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	p := persisted{Total: r.total, Batons: make([]*baton, 0, len(r.batons))}
	for _, b := range r.batons {
		p.Batons = append(p.Batons, b.clone())
	}
	r.dirty = false
	r.mu.Unlock()
	raw, err := json.Marshal(p)
	if err == nil {
		err = os.WriteFile(r.statePath+".tmp", raw, 0o600)
	}
	if err == nil {
		err = os.Rename(r.statePath+".tmp", r.statePath)
	}
	if err != nil {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
		log.Printf("state: save: %v", err)
	}
}

// ------------------------------------------------------------- cooldowns

// Two limits apply to a wave. The identity has a ready-at deadline: the full
// cooldown after a delivered wave, a short one after waving into an empty
// room. The address has a budget of delivered waves per cooldown window, so a
// fresh identity from the same address cannot reset the clock, while people
// sharing that address still each get a turn.

func (r *relay) pruneCooldowns(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, ready := range r.idReady {
		if !now.Before(ready) {
			delete(r.idReady, id)
		}
	}
	for key, waves := range r.addrWaves {
		if kept := r.recentLocked(waves, now); len(kept) == 0 {
			delete(r.addrWaves, key)
		} else {
			r.addrWaves[key] = kept
		}
	}
}

// recentLocked drops timestamps that have aged out of the cooldown window.
func (r *relay) recentLocked(waves []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(waves) && now.Sub(waves[i]) >= r.cooldown {
		i++
	}
	return waves[i:]
}

func (r *relay) remainingLocked(id, addr string, now time.Time) int {
	remaining := r.idReady[id].Sub(now)
	if waves := r.recentLocked(r.addrWaves[addr], now); len(waves) >= r.addrBudget {
		if wait := r.cooldown - now.Sub(waves[0]); wait > remaining {
			remaining = wait
		}
	}
	if remaining < 0 {
		remaining = 0
	}
	return int(math.Ceil(remaining.Seconds()))
}

// spendLocked records a wave. Only a delivered wave counts against the
// address budget: an undelivered one affected nobody.
func (r *relay) spendLocked(id, addr string, now time.Time, delivered bool) int {
	if !delivered {
		r.idReady[id] = now.Add(r.emptyCooldown)
		return int(math.Ceil(r.emptyCooldown.Seconds()))
	}
	r.idReady[id] = now.Add(r.cooldown)
	r.addrWaves[addr] = append(r.recentLocked(r.addrWaves[addr], now), now)
	return int(math.Ceil(r.cooldown.Seconds()))
}

func (r *relay) stateLocked(id, addr string, now time.Time) frame {
	f := frame{"type": "state", "remaining": r.remainingLocked(id, addr, now),
		"baton": r.batonFrameLocked(r.batonHeldByLocked(id))}
	if r.minClient != "" {
		f["minClient"] = r.minClient
	}
	return f
}

// A plugin is a git clone that only moves when its owner runs
// `omarchy plugin update`, so old widgets can stay out there indefinitely.
// The relay cannot update them, but it can say which version it still speaks
// to, and the widget turns that into a hint. Dotted digits only, so the value
// compares cleanly on the client and cannot carry anything else.
func validVersion(v string) bool {
	if v == "" {
		return false
	}
	for _, part := range strings.Split(v, ".") {
		if part == "" {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// -------------------------------------------------------------- identity

// Country, resolved at request time and then dropped. A trusted edge may say
// it outright: Cloudflare sets CF-IPCountry, and other proxies can be taught to
// set X-Country. Otherwise the caller's address is looked up in the range list
// loaded with -country-db, if any. With neither, everyone is simply "??".
func (r *relay) origin(req *http.Request) string {
	if !sharesRegion(req) {
		return "??"
	}
	if r.trusts(peerIP(req)) {
		if cc := headerCountry(req); cc != "??" {
			return cc
		}
	}
	return r.geo.lookup(r.clientIP(req))
}

// headerCountry trusts nothing by itself; callers check the peer first. The
// value is attacker-supplied at the edge and gets relayed verbatim to another
// user, so it is validated exactly.
func headerCountry(req *http.Request) string {
	for _, header := range []string{"CF-IPCountry", "X-Country"} {
		if cc := strings.ToUpper(strings.TrimSpace(req.Header.Get(header))); validCountry(cc) {
			return cc
		}
	}
	return "??"
}

func (r *relay) trusts(addr netip.Addr) bool {
	for _, prefix := range r.trustedProxies {
		if prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

func peerIP(req *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr.Unmap()
}

// The immediate peer must be trusted. For XFF, walk from the nearest proxy
// toward the caller and stop at the first untrusted hop, ignoring spoofed left entries.
func (r *relay) clientIP(req *http.Request) netip.Addr {
	peer := peerIP(req)
	if !r.trusts(peer) {
		return peer
	}
	if addr, err := netip.ParseAddr(strings.TrimSpace(req.Header.Get("CF-Connecting-IP"))); err == nil {
		return addr.Unmap()
	}
	chain := strings.Split(req.Header.Get("X-Forwarded-For"), ",")
	addr := peer
	for i := len(chain) - 1; i >= 0 && r.trusts(addr); i-- {
		next, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			return peer
		}
		addr = next.Unmap()
	}
	return addr
}

// A short-lived, non-reversible stand-in for the client address, used only for
// the address budget and the stream cap. The salt is regenerated every process
// start, so these hashes are not stable identifiers and cannot be correlated
// with anything outside this process's lifetime.
//
// IPv6 is keyed by its /64: privacy extensions hand a host a fresh address
// whenever it likes, and a single subscriber normally owns the whole /64, so
// anything narrower would make the budget trivially evadable.
func (r *relay) addrKey(req *http.Request) string {
	addr := r.clientIP(req)
	if addr.Is6() && !addr.Is4In6() {
		if prefix, err := addr.Prefix(64); err == nil {
			addr = prefix.Masked().Addr()
		}
	}
	sum := sha256.Sum256(append(r.salt, []byte(addr.String())...))
	return fmt.Sprintf("%x", sum[:12])
}

func identity(req *http.Request) (string, bool) {
	id := strings.TrimSpace(req.Header.Get("X-Baton-Id"))
	// Client-minted opaque token. Bounded and charset-checked so it cannot be
	// used to smuggle anything into a log line or a map key.
	if len(id) < 8 || len(id) > 64 {
		return "", false
	}
	for _, c := range id {
		isAlnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !isAlnum && c != '-' && c != '_' {
			return "", false
		}
	}
	return id, true
}

func sharesRegion(req *http.Request) bool {
	return req.Header.Get("X-Baton-Share-Region") != "0"
}

// ------------------------------------------------------------- delivery

func (r *relay) snapshot() (total int64, online int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total, len(r.clients)
}

func encode(f frame) []byte {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	return append(raw, '\n')
}

// Acceptance means queued, not acknowledged by the remote desktop. A wedged
// client is retired; handoffs only commit after a successful enqueue.
func (c *client) send(payload []byte) bool {
	if payload == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- payload:
		return true
	case <-c.done:
		return false
	default:
		c.retire()
		return false
	}
}

func (r *relay) broadcastStats() {
	total, online := r.snapshot()
	payload := encode(frame{"type": "stats", "total": total, "online": online})

	r.mu.Lock()
	targets := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		targets = append(targets, c)
	}
	r.mu.Unlock()

	for _, c := range targets {
		c.send(payload)
	}
}

// register/remove keep a dense pool; removal swaps the last entry into the gap.
// Both run under r.mu, including reconnect replacement. Registration fails
// only when the address already has its full quota of streams and this is not
// a reconnect of an identity it already holds.
func (r *relay) registerLocked(c *client) bool {
	previous := r.clients[c.id]
	if previous == nil && r.streamCap > 0 && r.streams[c.addr] >= r.streamCap {
		return false
	}
	if previous != nil {
		r.releaseStreamLocked(previous)
		previous.retire()
		c.index = previous.index
		r.connected[c.index] = c
	} else {
		c.index = len(r.connected)
		r.connected = append(r.connected, c)
	}
	r.streams[c.addr]++
	r.clients[c.id] = c
	return true
}

func (r *relay) removeLocked(c *client) {
	if r.clients[c.id] != c {
		return
	}
	last := r.connected[len(r.connected)-1]
	r.connected[c.index] = last
	last.index = c.index
	r.connected[len(r.connected)-1] = nil
	r.connected = r.connected[:len(r.connected)-1]
	r.releaseStreamLocked(c)
	delete(r.clients, c.id)
}

func (r *relay) releaseStreamLocked(c *client) {
	if r.streams[c.addr] <= 1 {
		delete(r.streams, c.addr)
	} else {
		r.streams[c.addr]--
	}
}

// Uniform cryptographic selection, excluding the sender. Assumes r.mu is held.
func (r *relay) pickRecipientLocked(senderID string, needsFreeHands bool) *client {
	count := len(r.connected)
	sender := r.clients[senderID]
	if sender != nil {
		count--
	}
	if count == 0 {
		return nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return nil
	}
	index := int(n.Int64())
	if sender != nil && index >= sender.index {
		index++
	}
	candidate := r.connected[index]
	eligible := func(c *client) bool {
		select {
		case <-c.done:
			return false
		default:
		}
		return c.id != senderID && (!needsFreeHands || r.batonHeldByLocked(c.id) == nil)
	}
	if eligible(candidate) {
		return candidate
	}
	// Rare fallback: occupied hands or a connection still being retired.
	// Choosing uniformly here preserves uniformity among eligible recipients.
	candidates := make([]*client, 0, count)
	for _, c := range r.connected {
		if eligible(c) {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	n, err = rand.Int(rand.Reader, big.NewInt(int64(len(candidates))))
	if err != nil {
		return nil
	}
	return candidates[n.Int64()]
}

// --------------------------------------------------------------- routes

func (r *relay) handleWave(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := identity(req)
	if !ok {
		http.Error(w, "bad identity", http.StatusBadRequest)
		return
	}
	addr, now := r.addrKey(req), time.Now()
	origin := r.origin(req)
	r.mu.Lock()
	respond := func(f frame) {
		// Ownership snapshots travel on the ordered stream, never in an HTTP
		// response that could arrive after a newer incoming baton.
		if sender := r.clients[id]; sender != nil {
			sender.send(encode(r.stateLocked(id, addr, now)))
		}
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(encode(f))
	}
	if remaining := r.remainingLocked(id, addr, now); remaining > 0 {
		respond(frame{"type": "cooldown", "remaining": remaining})
		return
	}
	b := r.batonHeldByLocked(id)
	mint := b == nil && len(r.batons) < batonTarget(len(r.clients))
	recipient := r.pickRecipientLocked(id, b != nil || mint)
	// When every stranger is holding a baton, send a plain wave and retain ours.
	if recipient == nil {
		recipient = r.pickRecipientLocked(id, false)
		mint = false
	}
	if recipient == nil {
		respond(frame{"type": "cooldown", "remaining": r.spendLocked(id, addr, now, false), "delivered": false})
		return
	}
	var next *baton
	if r.batonHeldByLocked(recipient.id) == nil {
		if b != nil {
			next = b.clone()
		} else if mint {
			next = newBaton(now)
		}
	}
	wave := frame{"type": "wave", "origin": origin}
	if next != nil {
		next.Hops++
		if origin != "??" {
			next.Countries[origin] = true
		}
		next.holder = recipient.id
		wave["baton"] = r.batonFrameLocked(next)
	}
	delivered := recipient.send(encode(wave))
	if delivered {
		if next != nil {
			r.batons[next.ID] = next
		}
		r.total++
		r.dirty = true
	}
	respond(frame{"type": "cooldown", "remaining": r.spendLocked(id, addr, now, delivered),
		"delivered": delivered, "passed": delivered && next != nil})
}

// handleBatons lists every baton that has ever lived, hardest-travelled first.
// No holder, country list, or sequence is exposed.
func (r *relay) handleBatons(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	rows := make([]map[string]any, 0, len(r.batons))
	for _, b := range r.batons {
		row := r.batonFrameLocked(b)
		if _, connected := r.clients[b.holder]; connected {
			row["held"] = true
		}
		rows = append(rows, row)
	}
	r.mu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["hops"].(int64) > rows[j]["hops"].(int64)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"batons": rows})
}

func (r *relay) handleStream(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := identity(req)
	if !ok {
		http.Error(w, "bad identity", http.StatusBadRequest)
		return
	}
	_, ok = w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	addr := r.addrKey(req)
	c := &client{id: id, addr: addr, out: make(chan []byte, sendBuffer), done: make(chan struct{})}

	r.mu.Lock()
	if !r.registerLocked(c) {
		r.mu.Unlock()
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	}
	c.send(encode(r.stateLocked(id, addr, time.Now())))
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.removeLocked(c)
		r.mu.Unlock()
		c.retire()
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	write := func(payload []byte) error {
		if err := controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		return controller.Flush()
	}
	total, online := r.snapshot()
	if err := write(encode(frame{"type": "stats", "total": total, "online": online})); err != nil {
		return
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-c.done:
			return
		case payload := <-c.out:
			if err := write(payload); err != nil {
				return
			}
		case <-ping.C:
			if err := write(encode(frame{"type": "ping"})); err != nil {
				return
			}
		}
	}
}

func (r *relay) handleStats(w http.ResponseWriter, req *http.Request) {
	total, online := r.snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(frame{"total": total, "online": online})
}

func (r *relay) handleHealth(w http.ResponseWriter, req *http.Request) {
	w.Write([]byte("ok\n"))
}

// --------------------------------------------------------------- main

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	statePath := flag.String("state", "", "path to the aggregate state file (empty disables persistence)")
	cooldown := flag.Duration("cooldown", defaultCooldown, "minimum time between delivered waves from one sender")
	emptyCooldown := flag.Duration("empty-cooldown", defaultEmptyCooldown, "wait after a wave that found nobody online")
	addrBudget := flag.Int("address-waves", defaultAddressWaves, "delivered waves allowed per address per cooldown window")
	streamCap := flag.Int("streams-per-address", defaultStreamsPerAddress, "open streams allowed per address (0 disables the cap)")
	proxyList := flag.String("trusted-proxies", "", "comma-separated proxy IPs/CIDRs allowed to set country and forwarding headers")
	geoList := flag.String("country-db", "", "comma-separated CSV range lists (start,end,country) for country lookup")
	minClient := flag.String("min-client", "", "oldest plugin version this relay supports, advertised to clients (empty advertises nothing)")
	flag.Parse()
	if *cooldown <= 0 {
		log.Fatal("cooldown must be positive")
	}
	if *emptyCooldown < 0 || *addrBudget < 1 || *streamCap < 0 {
		log.Fatal("empty-cooldown and streams-per-address must be non-negative; address-waves must be at least 1")
	}

	if *statePath != "" {
		if err := os.MkdirAll(filepath.Dir(*statePath), 0o755); err != nil {
			log.Fatalf("state dir: %v", err)
		}
	}

	r, err := newRelay(*statePath, *cooldown)
	if err != nil {
		log.Fatal(err)
	}
	r.emptyCooldown, r.addrBudget, r.streamCap = *emptyCooldown, *addrBudget, *streamCap
	if *minClient != "" {
		if !validVersion(*minClient) {
			log.Fatalf("min-client must be dotted digits like 0.4.0, got %q", *minClient)
		}
		r.minClient = *minClient
	}

	if paths := splitList(*geoList); len(paths) > 0 {
		geo, err := loadCountryDB(paths)
		if err != nil {
			log.Fatalf("country-db: %v", err)
		}
		r.geo = geo
		log.Printf("country-db: %d ranges loaded", geo.size())
	}

	for _, value := range splitList(*proxyList) {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, parseErr := netip.ParseAddr(value)
			if parseErr != nil {
				log.Fatalf("invalid trusted proxy: %s", value)
			}
			prefix = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		r.trustedProxies = append(r.trustedProxies, prefix)
	}
	srv := &http.Server{
		Addr:    *addr,
		Handler: r.routes(),
		// No WriteTimeout: /stream is meant to stay open indefinitely. Idle
		// keep-alive connections, which carry no stream, are closed.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	maintenanceDone := make(chan struct{})
	maintenanceStopped := make(chan struct{})
	go func() {
		defer close(maintenanceStopped)
		stats := time.NewTicker(statsInterval)
		save := time.NewTicker(30 * time.Second)
		rehome := time.NewTicker(rehomeInterval)
		defer stats.Stop()
		defer save.Stop()
		defer rehome.Stop()
		for {
			select {
			case <-stats.C:
				r.broadcastStats()
			case <-save.C:
				r.pruneCooldowns(time.Now())
				r.saveState()
			case <-maintenanceDone:
				return
			case <-rehome.C:
				r.rehomeOrphans()
			}
		}
	}()

	shutdownDone := make(chan struct{})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("shutting down")
		close(maintenanceDone)
		<-maintenanceStopped
		r.mu.Lock()
		for _, c := range r.clients {
			c.retire()
		}
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		r.saveState()
		close(shutdownDone)
	}()

	log.Printf("baton relay listening on %s (cooldown %s)", *addr, *cooldown)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
