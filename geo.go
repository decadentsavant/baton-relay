package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
)

// Country lookup without a geolocation service.
//
// The relay can load a plain list of IP ranges with a country code per range,
// sorted, and binary-search it at request time. That is the whole mechanism:
// no API calls, no vendor SDK, no per-request logging. The lookup runs on the
// caller's address in memory and only the two-letter result leaves this file.
//
// The accepted format is one range per line, no header:
//
//	1.0.0.0,1.0.0.255,AU
//	2001:200::,2001:200:ffff:ffff:ffff:ffff:ffff:ffff,JP
//
// That is the layout published by the ip-location-db project (the
// public-domain "user-country" files) and by DB-IP's free country list, so
// either can be dropped in. IPv4 and IPv6 may be mixed or split across files.
type countryDB struct {
	v4 []v4Range
	v6 []v6Range
}

type v4Range struct {
	start, end uint32
	cc         [2]byte
}

type v6Range struct {
	start, end [16]byte
	cc         [2]byte
}

func loadCountryDB(paths []string) (*countryDB, error) {
	db := &countryDB{}
	for _, path := range paths {
		if err := db.loadFile(path); err != nil {
			return nil, err
		}
	}
	if len(db.v4)+len(db.v6) == 0 {
		return nil, errors.New("country database is empty")
	}
	sort.Slice(db.v4, func(i, j int) bool { return db.v4[i].start < db.v4[j].start })
	sort.Slice(db.v6, func(i, j int) bool { return compare16(db.v6[i].start, db.v6[j].start) < 0 })
	return db, nil
}

func (db *countryDB) loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		startText, rest, ok := strings.Cut(text, ",")
		if !ok {
			return fmt.Errorf("%s:%d: expected start,end,country", path, line)
		}
		endText, ccText, ok := strings.Cut(rest, ",")
		if !ok {
			return fmt.Errorf("%s:%d: expected start,end,country", path, line)
		}
		start, err := netip.ParseAddr(strings.TrimSpace(startText))
		if err != nil {
			// A header row is the one non-range line worth tolerating.
			if line == 1 {
				continue
			}
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		end, err := netip.ParseAddr(strings.TrimSpace(endText))
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		cc := strings.ToUpper(strings.TrimSpace(ccText))
		if !validCountry(cc) {
			// Ranges the source could not attribute (often "ZZ" or blank) are
			// simply unknown to us as well.
			continue
		}
		start, end = start.Unmap(), end.Unmap()
		if start.Is4() != end.Is4() {
			return fmt.Errorf("%s:%d: mixed address families", path, line)
		}
		if start.Is4() {
			db.v4 = append(db.v4, v4Range{start: v4Key(start), end: v4Key(end), cc: [2]byte{cc[0], cc[1]}})
		} else {
			db.v6 = append(db.v6, v6Range{start: start.As16(), end: end.As16(), cc: [2]byte{cc[0], cc[1]}})
		}
	}
	return scanner.Err()
}

// lookup returns "??" for anything the list does not cover, including private
// and loopback addresses, which the public lists deliberately omit.
func (db *countryDB) lookup(addr netip.Addr) string {
	if db == nil || !addr.IsValid() {
		return "??"
	}
	addr = addr.Unmap()
	if addr.Is4() {
		ip := v4Key(addr)
		i := sort.Search(len(db.v4), func(i int) bool { return db.v4[i].start > ip })
		if i > 0 && db.v4[i-1].end >= ip {
			return string(db.v4[i-1].cc[:])
		}
		return "??"
	}
	ip := addr.As16()
	i := sort.Search(len(db.v6), func(i int) bool { return compare16(db.v6[i].start, ip) > 0 })
	if i > 0 && compare16(db.v6[i-1].end, ip) >= 0 {
		return string(db.v6[i-1].cc[:])
	}
	return "??"
}

func (db *countryDB) size() int {
	if db == nil {
		return 0
	}
	return len(db.v4) + len(db.v6)
}

func v4Key(addr netip.Addr) uint32 {
	b := addr.As4()
	return binary.BigEndian.Uint32(b[:])
}

func compare16(a, b [16]byte) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// validCountry admits exactly two ASCII capitals. It is checked byte by byte
// rather than with a "AA" <= cc <= "ZZ" range, which happily admits things like
// "A[". Country codes are relayed verbatim to another user, so it is worth being
// exact. Cloudflare's sentinels for "unknown" and "Tor exit" are excluded too.
func validCountry(cc string) bool {
	if len(cc) != 2 {
		return false
	}
	if cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
		return false
	}
	return cc != "XX" && cc != "T1" && cc != "ZZ"
}
