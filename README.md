# Baton relay

The server for [Baton, the Omarchy bar plugin](https://github.com/decadentsavant/baton).
This repository contains the Go relay, public website, and deployment files.
Desktop users only need the client plugin; installing it connects to the public
relay at `https://relay.baton.buzz`. Hosting this server is optional.

MIT licensed; see [LICENSE](LICENSE).

Stdlib-only Go, with a small embedded public page and no frontend build step.
One binary, one optional JSON file of aggregate state, and optionally two CSV
files of IP ranges so waves can say which country they came from.

## Run it

Requires Go 1.23 or newer to build. The service uses only the Go standard
library; there are no third-party Go modules or JavaScript dependencies.
Clone this repository and run commands from its root:

    git clone https://github.com/decadentsavant/baton-relay.git
    cd baton-relay

    go build -o baton-relay . && ./baton-relay -addr :8080 -state ./state.json

Or with Docker (runs as an unprivileged user, state in a named volume):

    docker build -t baton-relay .
    docker run -p 8080:8080 -v baton-state:/var/lib/baton baton-relay

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | Listen address. |
| `-state` | empty | Aggregate state file. Empty disables persistence. |
| `-cooldown` | `1h` | Wait after a delivered wave. |
| `-empty-cooldown` | `1m` | Wait after a wave that found nobody online. |
| `-address-waves` | `4` | Delivered waves allowed per address per cooldown window. |
| `-streams-per-address` | `8` | Open streams allowed per address. `0` disables the cap. |
| `-trusted-proxies` | empty | Proxy IPs/CIDRs allowed to set forwarding and country headers. |
| `-country-db` | empty | CSV range lists (`start,end,country`) for country lookup. |
| `-min-client` | empty | Oldest plugin version this relay supports, e.g. `0.4.0`. Empty advertises nothing. |

## Deploying on a small VPS

The `deploy/` directory is a complete recipe for one machine running Caddy
and the relay, which is how the public relay runs:

- `deploy/baton-relay.service`: a hardened systemd unit, listening on loopback
  only and trusting only loopback to forward the caller's address.
- `deploy/Caddyfile`: automatic HTTPS, and it strips any country header a
  client tries to supply.
- `deploy/fetch-country-db.sh`: downloads the public-domain IP-to-country
  lists the relay loads with `-country-db`.

The unit file's header comment has the install commands. No Cloudflare, no
GeoIP subscription, no API keys.

## Country resolution

Waves say which country they came from, never anything finer. The relay works
that out at request time, in one of two ways, and then forgets the address:

1. **A trusted edge says so.** If the immediate peer matches
   `-trusted-proxies`, `CF-IPCountry` or `X-Country` is believed. That is for
   people who already run a CDN or a GeoIP-aware proxy.
2. **A range list.** Otherwise the caller's address is binary-searched in the
   lists loaded with `-country-db`. The accepted format is one range per line,
   `start,end,country`, no header, IPv4 and IPv6 in the same or separate files.
   The public-domain `user-country` files from
   [ip-location-db](https://github.com/sapics/ip-location-db) are that shape
   and are what `deploy/fetch-country-db.sh` fetches. About 560k ranges load in
   well under a second and cost the process roughly 50 MB.

With neither, everyone is simply `??`, which the widget shows as "somewhere".
A client that sets `X-Baton-Share-Region: 0` is always `??`, whatever the relay
knows.

The address a lookup runs on is whatever `-trusted-proxies` allows: the socket
peer by default, or the address a trusted proxy forwards. For `X-Forwarded-For`
the relay walks right to left and stops at the first untrusted hop, so a client
cannot prepend a fake address. Never trust all addresses. If a CDN connects
directly, list its published proxy networks and restrict origin ingress to them.

## Limits

Everything below exists to keep one bit worth something. None of it needs an
account.

- **Cooldown.** A delivered wave costs the sender the full cooldown. A wave
  that found nobody online affected nobody, so it costs `-empty-cooldown`, and
  the response says `"delivered": false` so the widget can say so.
- **Address budget.** Each network address gets `-address-waves` delivered
  waves per cooldown window. Rotating the identity token does not reset the
  clock, while a household or office behind one IP still gets several turns.
  IPv6 is keyed by /64, since privacy extensions rotate the host part freely.
- **Stream cap.** Open streams count as "online" and receive waves, so one
  address may hold `-streams-per-address` of them. Reconnecting an identity
  the address already holds is not a new stream.

Addresses are never stored. They are hashed with a salt regenerated on every
process start, kept in memory only while a budget or a stream is live, and the
hash cannot be correlated with anything outside the process's lifetime.

## Protocol

Newline-delimited JSON, not SSE. Line splitting is something Quickshell's
`SplitParser` already does, so SSE framing would have been ceremony.

`GET /stream` — long-lived. Requires `X-Baton-Id`. Emits:

    {"type":"stats","total":120491,"online":3847}
    {"type":"state","remaining":1800,"baton":null,"minClient":"0.4.0"}
    {"type":"wave","origin":"PL"}
    {"type":"wave","origin":"PL","baton":{"id":"a3f…","born":"2026-09-04T12:00:00Z","hops":412,"countries":23}}
    {"type":"baton","baton":{…}}          // an orphan re-homed to you
    {"type":"ping"}

Returns `429` when the address already holds its full quota of streams.

`POST /wave` — requires `X-Baton-Id`. Responds with one frame:

    {"type":"cooldown","remaining":3600,"delivered":true,"passed":true}
    {"type":"cooldown","remaining":60,"delivered":false}      // nobody online
    {"type":"cooldown","remaining":1800}                       // still cooling down

`delivered` means accepted into the recipient's outbound queue, not acknowledged
by the remote desktop. Congested clients are retired and their handoffs do not
commit. Individual stream writes have a 10-second deadline. `passed` says
whether a baton actually left your hands.

`state` is sent on connection and after the sender's wave attempt; it carries
the authoritative cooldown and current baton (or null). Ownership changes must
be applied from this ordered stream, not a potentially delayed POST response.
`minClient` is present only when the relay runs with `-min-client`.

## Client versions

The plugin is a git clone in the user's Omarchy config. Nothing pulls it
automatically; it moves only when its owner runs `omarchy plugin update`, so
any version ever published may still be connecting. The relay has no way to
update a widget, and it does not reject old ones. What it can do is state the
oldest version it still fully supports, with `-min-client`. A widget that
reads `minClient` from the `state` frame and finds itself behind adds an
update hint to its tooltip and sends one notification per session. Widgets
older than 0.4.0 ignore the field, so raise the floor only when the protocol
actually changes, and keep old frames readable for as long as you can.

`GET /` — public listing and installation instructions.
`GET /b/<id>` — shareable baton page with server-rendered social metadata.
`GET /batons` — every baton that has ever lived, hardest-travelled first.
`GET /stats` — `{"total":…,"online":…}`. `GET /healthz` — `ok`.
`GET /map` — compatibility redirect to the listing.

Headers: `X-Baton-Id` is an opaque client-minted token (8–64 chars, `[A-Za-z0-9_-]`).
`X-Baton-Share-Region: 0` makes your waves arrive as `??`.

## Batons

A baton is a wave that remembers. It carries three facts: when it was born, how
many times a human deliberately passed it on, and how many distinct countries it
has been through.

It does **not** carry an ordered trail. A sequence of countries with timestamps
is a movement log, and in a community where one country might hold a dozen
users, a log like that says something about a person. A count says something
only about the baton. The relay keeps an unordered set of country codes per
baton so distinct ones can be counted at all; that set is persisted so the
count survives restarts, and the public API exposes only its size.

Batons are born from waves: if fewer batons exist than the target of one per 25
connected clients (minimum one with two users, maximum 64), the next eligible
wave mints one. Orphans count toward the target. If you are holding a baton,
your wave passes it on.

**Batons are re-homed, not killed.** An earlier design let a baton die with its
holder, which reads well and does not survive contact with reality: the
cooldown is an hour and laptops sleep, so the median baton would have died at
one hop and "alive since" would have meant nothing. A baton whose holder
disconnects is orphaned and handed to someone else on the next 90-second tick.
Recipients must have free hands to receive a baton. When all strangers are
holding one, a plain wave is sent and the sender keeps theirs. Excess orphans
wait for free hands; no baton is destroyed.

Re-homing deliberately does not count as a hop: a hop is a person deciding to
pass something on, not the scheduler handing you something.

## What it stores

`state.json` holds the total wave count and one record per baton: id, birth
time, hop count, and the country set behind the count.

Holders are **not** persisted. The field is unexported so `encoding/json` cannot
write it. Every baton comes back as an orphan after a restart.

Connected clients live in memory and are forgotten on disconnect. The relay
sees sender and recipient while routing a wave and keeps no history of pairs.

State saves deep-copy baton data under the lock and serialize disk writes.
Failed saves are retried, and shutdown stops new work before the final save.

## Checks

    go test -race ./...
    go test -run '^$' -bench BenchmarkRecipient -benchmem

For an isolated two-client protocol demo after building:

    ./dev/loopback.sh

To test the real Omarchy widget against this server, keep the client checkout
next to this one and follow its [preview guide](https://github.com/decadentsavant/baton/blob/main/docs/PREVIEW.md).
