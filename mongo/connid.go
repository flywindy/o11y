package mongo

import "strings"

// normalizeConnectionID strips the per-connection counter the MongoDB driver
// appends to the connection identifier carried on command events, leaving the
// plain "host:port" that otelmongo expects to parse.
//
// The v2 driver builds the identifier as fmt.Sprintf("%s[-%d]", addr, n)
// (x/mongo/driver/topology/connection.go), where n comes from a process-global
// counter incremented for every connection the process ever opens — pool
// connections, server monitors and RTT monitors alike. otelmongo derives
// network.peer.address and network.peer.port from that string with
// net.SplitHostPort, which rejects the trailing bracket ("unexpected '[' in
// address") and falls back to using the whole identifier as the address and a
// hardcoded 27017 as the port. Without this normalization the address label
// therefore carries the counter — so every connection a pod opens over its
// lifetime adds a permanent series to db.client.operation.duration, until the
// stream hits its cardinality limit and collapses into the overflow series —
// and the port label is wrong whenever MongoDB does not listen on 27017.
//
// Only the bracketed suffix is unbounded; everything before it is the topology
// address, bounded by the number of servers in the deployment. The suffix is
// removed only when it matches the driver's shape exactly — a bracketed run of
// decimal digits, optionally signed — so a bare IPv6 address ("[::1]:27017",
// and the degenerate "[::1]") and a unix socket path are left untouched.
func normalizeConnectionID(id string) string {
	if !strings.HasSuffix(id, "]") {
		return id
	}
	open := strings.LastIndexByte(id, '[')
	if open <= 0 {
		// Either there is no opening bracket, or the value is entirely
		// bracketed (a bare IPv6 host), which leaves no address behind.
		return id
	}
	if !isConnectionCounter(id[open+1 : len(id)-1]) {
		return id
	}
	return id[:open]
}

// isConnectionCounter reports whether s is the body of the driver's connection
// suffix: a run of decimal digits, optionally prefixed with the "-" the driver
// writes today. Anything else is left in place rather than guessed at, so an
// unrecognized future format degrades to the current behavior instead of
// silently discarding part of the address.
func isConnectionCounter(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
