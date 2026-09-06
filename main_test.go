package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRelay(t *testing.T) *relay {
	t.Helper()
	r, e := newRelay("", time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func addClient(r *relay, id string) *client {
	c := &client{id: id, addr: "addr-" + id, out: make(chan []byte, sendBuffer), done: make(chan struct{})}
	r.registerLocked(c)
	return c
}
func post(r *relay, id, addr string, headers ...string) frame {
	q := httptest.NewRequest("POST", "/wave", nil)
	q.Header.Set("X-Baton-Id", id)
	q.RemoteAddr = addr
	for i := 0; i+1 < len(headers); i += 2 {
		q.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	r.handleWave(w, q)
	var f frame
	json.Unmarshal(w.Body.Bytes(), &f)
	return f
}
func readFrame(t *testing.T, c *client) frame {
	t.Helper()
	select {
	case raw := <-c.out:
		var f frame
		json.Unmarshal(raw, &f)
		return f
	default:
		t.Fatal("no frame queued")
		return nil
	}
}
func TestEmptyCrowdCostsOnlyAMinute(t *testing.T) {
	r := testRelay(t)
	f := post(r, "alicealice", "192.0.2.1:123")
	if f["delivered"] != false || f["remaining"] != float64(60) {
		t.Fatal(f)
	}
	f = post(r, "alicealice", "192.0.2.1:123")
	if _, retried := f["delivered"]; retried || f["remaining"].(float64) < 59 {
		t.Fatal("short cooldown not enforced", f)
	}
	// Nobody was affected, so the address budget is untouched.
	if len(r.addrWaves) != 0 {
		t.Fatal("undelivered wave charged the address")
	}
}
func TestAddressBudgetSharesFairly(t *testing.T) {
	r := testRelay(t)
	addClient(r, "receiverreceiver")
	for i := 0; i < defaultAddressWaves; i++ {
		if f := post(r, fmt.Sprintf("roommate-%d", i), "192.0.2.1:123"); f["delivered"] != true {
			t.Fatal(i, f)
		}
	}
	f := post(r, "roommate-extra", "192.0.2.1:123")
	if _, delivered := f["delivered"]; delivered || f["remaining"].(float64) < 3599 {
		t.Fatal("budget not enforced", f)
	}
	if f := post(r, "neighbour-1", "192.0.2.2:123"); f["delivered"] != true {
		t.Fatal("other address blocked", f)
	}
}
func TestIPv6KeyedBySubnet(t *testing.T) {
	r := testRelay(t)
	key := func(addr string) string {
		q := httptest.NewRequest("POST", "/wave", nil)
		q.RemoteAddr = addr
		return r.addrKey(q)
	}
	if key("[2001:db8:1:2::1]:1") != key("[2001:db8:1:2:ffff::9]:1") {
		t.Fatal("same /64 keyed differently")
	}
	if key("[2001:db8:1:2::1]:1") == key("[2001:db8:1:3::1]:1") || key("192.0.2.1:1") == key("192.0.2.2:1") {
		t.Fatal("distinct addresses share a key")
	}
}
func TestUntrustedCountryHeaderIgnored(t *testing.T) {
	r := testRelay(t)
	bob := addClient(r, "bobbobbob")
	post(r, "alicealice", "192.0.2.1:123", "X-Country", "PL", "CF-IPCountry", "SE")
	if f := readFrame(t, bob); f["origin"] != "??" {
		t.Fatal("spoofed country relayed", f)
	}
	r.trustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
	post(r, "carolcarol", "192.0.2.1:123", "X-Country", "pl")
	if f := readFrame(t, bob); f["origin"] != "PL" {
		t.Fatal("trusted country dropped", f)
	}
	post(r, "davedavedave", "192.0.2.1:123", "X-Country", "PL", "X-Baton-Share-Region", "0")
	if f := readFrame(t, bob); f["origin"] != "??" {
		t.Fatal("opt-out ignored", f)
	}
}
func TestCountryHeaderValidation(t *testing.T) {
	for value, want := range map[string]string{"pl": "PL", "XX": "??", "T1": "??", "A[": "??", "USA": "??", "": "??"} {
		q := httptest.NewRequest("POST", "/wave", nil)
		q.Header.Set("X-Country", value)
		if got := headerCountry(q); got != want {
			t.Fatalf("%q: got %q want %q", value, got, want)
		}
	}
}
func TestCountryDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "country.csv")
	os.WriteFile(path, []byte("start,end,country\n1.0.0.0,1.0.0.255,AU\n1.0.1.0,1.0.3.255,cn\n9.0.0.0,9.255.255.255,ZZ\n2001:200::,2001:200:ffff:ffff:ffff:ffff:ffff:ffff,JP\n"), 0o600)
	db, err := loadCountryDB([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]string{"1.0.0.7": "AU", "1.0.2.1": "CN", "1.0.4.0": "??", "9.1.1.1": "??",
		"2001:200::1": "JP", "2001:201::1": "??", "::ffff:1.0.0.1": "AU", "127.0.0.1": "??"} {
		if got := db.lookup(netip.MustParseAddr(addr)); got != want {
			t.Fatalf("%s: got %s want %s", addr, got, want)
		}
	}
	if (*countryDB)(nil).lookup(netip.MustParseAddr("1.0.0.1")) != "??" {
		t.Fatal("nil db must be unknown")
	}
	if _, err := loadCountryDB([]string{filepath.Join(t.TempDir(), "missing.csv")}); err == nil {
		t.Fatal("missing file accepted")
	}

	r := testRelay(t)
	r.geo = db
	bob := addClient(r, "bobbobbob")
	post(r, "alicealice", "1.0.0.9:123", "X-Country", "SE")
	if f := readFrame(t, bob); f["origin"] != "AU" {
		t.Fatal("untrusted peer should be looked up, not believed", f)
	}
	r.trustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	post(r, "carolcarol", "127.0.0.1:123", "X-Forwarded-For", "1.0.2.2")
	if f := readFrame(t, bob); f["origin"] != "CN" {
		t.Fatal("forwarded address not looked up", f)
	}
}
func TestIdentityValidation(t *testing.T) {
	for value, want := range map[string]bool{"alicealice": true, "short": false, "has space!": false, "ok-token_1": true,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false} {
		q := httptest.NewRequest("POST", "/wave", nil)
		q.Header.Set("X-Baton-Id", value)
		if _, ok := identity(q); ok != want {
			t.Fatalf("%q: got %v", value, ok)
		}
	}
}
func TestBatonTarget(t *testing.T) {
	for online, want := range map[int]int{0: 0, 1: 0, 2: 1, 24: 1, 50: 2, 5000: 64} {
		if got := batonTarget(online); got != want {
			t.Fatalf("%d online: got %d want %d", online, got, want)
		}
	}
}
func TestStreamCapPerAddress(t *testing.T) {
	r := testRelay(t)
	r.streamCap = 2
	s := httptest.NewServer(r.routes())
	defer s.Close()
	open := func(id string) *http.Response {
		q, _ := http.NewRequest("GET", s.URL+"/stream", nil)
		q.Header.Set("X-Baton-Id", id)
		p, e := s.Client().Do(q)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	first, second := open("alicealice"), open("bobbobbob")
	defer first.Body.Close()
	defer second.Body.Close()
	third := open("carolcarol")
	defer third.Body.Close()
	if first.StatusCode != 200 || second.StatusCode != 200 || third.StatusCode != http.StatusTooManyRequests {
		t.Fatal(first.StatusCode, second.StatusCode, third.StatusCode)
	}
	// A reconnect of an identity already counted is not a new stream.
	again := open("alicealice")
	defer again.Body.Close()
	if again.StatusCode != 200 {
		t.Fatal("reconnect refused", again.StatusCode)
	}
	first.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.clients)
		r.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	third.Body.Close()
	r.mu.Lock()
	streams := 0
	for _, n := range r.streams {
		streams += n
	}
	r.mu.Unlock()
	if streams != 2 {
		t.Fatal("stream accounting drifted", streams)
	}
}
func TestBatonHandoffAndFullHands(t *testing.T) {
	r := testRelay(t)
	addClient(r, "alicealice")
	addClient(r, "bobbobbob")
	b := newBaton(time.Now())
	b.holder = "alicealice"
	r.batons[b.ID] = b
	f := post(r, "alicealice", "192.0.2.1:123")
	if f["passed"] != true || r.batons[b.ID].holder != "bobbobbob" || r.batons[b.ID].Hops != 1 {
		t.Fatal(f)
	}
	other := newBaton(time.Now())
	other.holder = "alicealice"
	r.batons[other.ID] = other
	f = post(r, "bobbobbob", "192.0.2.2:123")
	if f["delivered"] != true || f["passed"] != false || r.batons[b.ID].holder != "bobbobbob" {
		t.Fatal(f)
	}
}
func TestCongestedHandoffDoesNotCommit(t *testing.T) {
	r := testRelay(t)
	addClient(r, "alicealice")
	c := addClient(r, "bobbobbob")
	for i := 0; i < sendBuffer; i++ {
		c.out <- []byte(`{}`)
	}
	b := newBaton(time.Now())
	b.holder = "alicealice"
	r.batons[b.ID] = b
	f := post(r, "alicealice", "192.0.2.1:123")
	if f["delivered"] != false || r.total != 0 || b.Hops != 0 || b.holder != "alicealice" {
		t.Fatal(f, b)
	}
	select {
	case <-c.done:
	default:
		t.Fatal("not retired")
	}
}
func TestRehomeKeepsExcessOrphans(t *testing.T) {
	r := testRelay(t)
	addClient(r, "alicealice")
	for i := 0; i < 10; i++ {
		b := newBaton(time.Now())
		r.batons[b.ID] = b
	}
	r.rehomeOrphans()
	held := 0
	for _, b := range r.batons {
		if b.holder != "" {
			held++
		}
		if b.Hops != 0 {
			t.Fatal("rehome counted hop")
		}
	}
	if held != 1 || len(r.batons) != 10 {
		t.Fatal(held)
	}
	addClient(r, "bobbobbob")
	post(r, "bobbobbob", "192.0.2.2:123")
	if len(r.batons) != 10 {
		t.Fatal("orphan backlog minted more")
	}
}
func TestCooldownPruning(t *testing.T) {
	r := testRelay(t)
	now := time.Now()
	r.idReady["expired"] = now.Add(-time.Second)
	r.idReady["active"] = now.Add(time.Minute)
	r.addrWaves["expired"] = []time.Time{now.Add(-time.Hour)}
	r.addrWaves["active"] = []time.Time{now.Add(-time.Hour), now.Add(-time.Minute)}
	r.pruneCooldowns(now)
	if len(r.idReady) != 1 || r.idReady["active"].IsZero() || len(r.addrWaves) != 1 || len(r.addrWaves["active"]) != 1 {
		t.Fatal("bad pruning", r.idReady, r.addrWaves)
	}
}
func TestProxyTrust(t *testing.T) {
	r := testRelay(t)
	q := httptest.NewRequest("POST", "/wave", nil)
	q.RemoteAddr = "192.0.2.1:12"
	q.Header.Set("CF-Connecting-IP", "198.51.100.1")
	if r.clientIP(q).String() != "192.0.2.1" {
		t.Fatal("untrusted header accepted")
	}
	r.trustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	if r.clientIP(q).String() != "198.51.100.1" {
		t.Fatal("trusted header ignored")
	}
	q.Header.Del("CF-Connecting-IP")
	q.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.1, 192.0.2.2")
	if r.clientIP(q).String() != "198.51.100.1" {
		t.Fatal("spoofed XFF accepted")
	}
}
func TestSaveRetriesAndExcludesHolders(t *testing.T) {
	r := testRelay(t)
	r.statePath = filepath.Join(t.TempDir(), "missing", "state.json")
	b := newBaton(time.Now())
	b.holder = "private-holder"
	b.Countries["PL"] = true
	r.batons[b.ID] = b
	r.dirty = true
	r.saveState()
	if !r.dirty {
		t.Fatal("failed save lost dirty")
	}
	os.MkdirAll(filepath.Dir(r.statePath), 0700)
	r.saveState()
	raw, e := os.ReadFile(r.statePath)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), "private-holder") || r.dirty {
		t.Fatal(string(raw))
	}
	loaded, e := newRelay(r.statePath, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.batons[b.ID].holder != "" || !loaded.batons[b.ID].Countries["PL"] {
		t.Fatal("bad restored state")
	}
}
func TestConcurrentSave(t *testing.T) {
	r := testRelay(t)
	r.statePath = filepath.Join(t.TempDir(), "state.json")
	b := newBaton(time.Now())
	r.batons[b.ID] = b
	r.dirty = true
	var wg sync.WaitGroup
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				r.mu.Lock()
				b.Hops++
				b.Countries[fmt.Sprint(i)] = true
				r.total++
				r.dirty = true
				r.mu.Unlock()
				r.saveState()
			}
		}()
	}
	wg.Wait()
	r.saveState()
	loaded, e := newRelay(r.statePath, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.total != 450 || loaded.batons[b.ID].Hops != 450 {
		t.Fatal("lost updates")
	}
}
func TestReconnectRestoresState(t *testing.T) {
	r := testRelay(t)
	b := newBaton(time.Now())
	b.holder = "alicealice"
	r.batons[b.ID] = b
	r.idReady["alicealice"] = time.Now().Add(time.Hour)
	s := httptest.NewServer(r.routes())
	defer s.Close()
	open := func() *http.Response {
		q, _ := http.NewRequest("GET", s.URL+"/stream", nil)
		q.Header.Set("X-Baton-Id", "alicealice")
		p, e := s.Client().Do(q)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	first := open()
	defer first.Body.Close()
	reader := bufio.NewReader(first.Body)
	reader.ReadBytes('\n')
	line, e := reader.ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	var f frame
	json.Unmarshal(line, &f)
	if f["type"] != "state" || f["remaining"].(float64) < 3599 || f["baton"].(map[string]any)["id"] != b.ID {
		t.Fatal(string(line))
	}
	second := open()
	defer second.Body.Close()
	r.mu.Lock()
	n := len(r.clients)
	r.mu.Unlock()
	if n != 1 {
		t.Fatal(n)
	}
}
func TestPublicPages(t *testing.T) {
	r := testRelay(t)
	b := newBaton(time.Now().Add(-21 * 24 * time.Hour))
	b.Hops = 412
	b.Countries["PL"] = true
	b.holder = "SECRET"
	r.batons[b.ID] = b
	for _, path := range []string{"/", "/b/" + b.ID, "/assets/style.css", "/assets/site.js", "/assets/favicon.svg", "/batons"} {
		w := httptest.NewRecorder()
		r.routes().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatal(path, w.Code)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal(path, "missing security headers")
		}
	}
	w := httptest.NewRecorder()
	r.routes().ServeHTTP(w, httptest.NewRequest("GET", "/b/missing", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func BenchmarkRecipient(b *testing.B) {
	for _, size := range []int{100, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			r, _ := newRelay("", time.Hour)
			for i := 0; i < size; i++ {
				addClient(r, fmt.Sprint(i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.pickRecipientLocked("0", false)
			}
		})
	}
}

func TestPoolReplacementAndRemoval(t *testing.T) {
	r := testRelay(t)
	old := addClient(r, "alicealice")
	bob := addClient(r, "bobbobbob")
	addClient(r, "carolcarol")
	replacement := addClient(r, "alicealice")
	r.removeLocked(old)
	if len(r.connected) != 3 || r.clients["alicealice"] != replacement {
		t.Fatal("old stream removed replacement")
	}
	r.removeLocked(bob)
	for i, c := range r.connected {
		if c.index != i || r.clients[c.id] != c {
			t.Fatal("broken pool")
		}
	}
	for i := 0; i < 100; i++ {
		if c := r.pickRecipientLocked("alicealice", false); c == nil || c.id != "carolcarol" {
			t.Fatal("invalid recipient")
		}
	}
}

func TestPublicSingularLabels(t *testing.T) {
	r := testRelay(t)
	b := newBaton(time.Now().Add(-time.Hour))
	b.Hops = 1
	b.Countries["PL"] = true
	r.batons[b.ID] = b
	w := httptest.NewRecorder()
	r.routes().ServeHTTP(w, httptest.NewRequest("GET", "/b/"+b.ID, nil))
	body := w.Body.String()
	if !strings.Contains(body, "Baton · 1 hop · 1 country") || !strings.Contains(body, "alive 1 hour") {
		t.Fatal(body)
	}
}

func TestAgeLabelMatchesWidget(t *testing.T) {
	day := 24 * time.Hour
	for age, want := range map[time.Duration]string{10 * time.Second: "just now", 5 * time.Minute: "5 minutes",
		time.Hour: "1 hour", 3 * day: "3 days", 21 * day: "3 weeks", 200 * day: "6 months"} {
		if got := ageLabel(age); got != want {
			t.Fatalf("%s: got %q want %q", age, got, want)
		}
	}
}
func TestMinClientAdvertised(t *testing.T) {
	r := testRelay(t)
	if _, ok := r.stateLocked("alicealice", "addr", time.Now())["minClient"]; ok {
		t.Fatal("minClient advertised without a floor")
	}
	r.minClient = "0.4.0"
	s := httptest.NewServer(r.routes())
	defer s.Close()
	q, _ := http.NewRequest("GET", s.URL+"/stream", nil)
	q.Header.Set("X-Baton-Id", "alicealice")
	p, e := s.Client().Do(q)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Body.Close()
	reader := bufio.NewReader(p.Body)
	reader.ReadBytes('\n')
	line, e := reader.ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	var f frame
	json.Unmarshal(line, &f)
	if f["type"] != "state" || f["minClient"] != "0.4.0" {
		t.Fatal(string(line))
	}
	f = post(r, "alicealice", "192.0.2.1:123")
	if _, ok := f["minClient"]; ok {
		t.Fatal("wave reply should not carry minClient; it travels on the stream")
	}
	for v, want := range map[string]bool{"0.4.0": true, "1": true, "0.4": true, "": false, "v0.4.0": false, "0..4": false, "0.4.": false, "0.4.0-rc1": false} {
		if validVersion(v) != want {
			t.Fatalf("validVersion(%q) = %v", v, !want)
		}
	}
}
