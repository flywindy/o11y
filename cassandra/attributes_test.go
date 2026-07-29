package cassandra

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseOperationAndTable(t *testing.T) {
	cases := []struct {
		statement string
		operation string
		keyspace  string
		table     string
		span      string
	}{
		{"SELECT id FROM messages_by_room WHERE room_id = ?", "SELECT", "", "messages_by_room", "cassandra.SELECT messages_by_room"},
		{"INSERT INTO chat.rooms (id) VALUES (?)", "INSERT", "chat", "rooms", "cassandra.INSERT rooms"},
		{"UPDATE rooms SET name = ? WHERE id = ?", "UPDATE", "", "rooms", "cassandra.UPDATE rooms"},
		{"DELETE FROM rooms WHERE id = ?", "DELETE", "", "rooms", "cassandra.DELETE rooms"},
		{"select count(*) from messages", "SELECT", "", "messages", "cassandra.SELECT messages"},
		{"INSERT INTO rooms(id,name) VALUES (?,?)", "INSERT", "", "rooms", "cassandra.INSERT rooms"},
		{"SELECT body FROM o11y_examples.events WHERE id = ?", "SELECT", "o11y_examples", "events", "cassandra.SELECT events"},
		{"UPDATE chat.rooms SET name = ? WHERE id = ?", "UPDATE", "chat", "rooms", "cassandra.UPDATE rooms"},
		{"INSERT INTO \"chat\".\"rooms\" (id) VALUES (?)", "INSERT", "chat", "rooms", "cassandra.INSERT rooms"},
		{"TRUNCATE rooms", "TRUNCATE", "", "rooms", "cassandra.TRUNCATE rooms"},
		{"TRUNCATE chat.rooms", "TRUNCATE", "chat", "rooms", "cassandra.TRUNCATE rooms"},
		{"TRUNCATE TABLE chat.rooms", "TRUNCATE", "chat", "rooms", "cassandra.TRUNCATE rooms"},
		{"CREATE TABLE IF NOT EXISTS o11y_examples.events (id text PRIMARY KEY, body text)", "CREATE", "o11y_examples", "events", "cassandra.CREATE events"},
		{"ALTER TABLE chat.rooms ADD col text", "ALTER", "chat", "rooms", "cassandra.ALTER rooms"},
		{"DROP TABLE IF EXISTS chat.rooms", "DROP", "chat", "rooms", "cassandra.DROP rooms"},
		{"CREATE KEYSPACE IF NOT EXISTS o11y_examples WITH replication = {'class':'SimpleStrategy'}", "CREATE", "o11y_examples", "", "cassandra.CREATE"},
		{"CREATE INDEX idx ON chat.rooms (name)", "CREATE", "", "", "cassandra.CREATE"},
		{"-- routing: room-read\nSELECT id FROM messages_by_room WHERE room_id = ?", "SELECT", "", "messages_by_room", "cassandra.SELECT messages_by_room"},
		{"/* app=chat */ INSERT INTO chat.rooms (id) VALUES (?)", "INSERT", "chat", "rooms", "cassandra.INSERT rooms"},
		{"", "", "", "", "cassandra"},
	}
	for _, c := range cases {
		t.Run(c.statement, func(t *testing.T) {
			op, ks, tbl := parseStatement(c.statement)
			assert.Equal(t, c.operation, op)
			assert.Equal(t, c.keyspace, ks, "parsed keyspace qualifier")
			assert.Equal(t, c.table, tbl)
			assert.Equal(t, c.span, spanName(op, tbl))
		})
	}
}

func TestContactPoint(t *testing.T) {
	cases := []struct {
		name        string
		hosts       []string
		defaultPort int
		wantHost    string
		wantPort    int
	}{
		{"host only uses default port", []string{"10.0.0.1"}, 9042, "10.0.0.1", 9042},
		{"host:port overrides default", []string{"10.0.0.1:9100"}, 9042, "10.0.0.1", 9100},
		{"first contact point wins", []string{"a", "b"}, 9042, "a", 9042},
		{"empty hosts", nil, 9042, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contactPoint(c.hosts, c.defaultPort)
			assert.Equal(t, c.wantHost, got.host)
			assert.Equal(t, c.wantPort, got.port)
		})
	}
}

// A CQL identifier may be quoted and contain whitespace. parseStatement
// tokenizes on whitespace, so such an identifier arrives already split; the
// table must then be omitted rather than reported as the truncated first token,
// which would be a confident, wrong table name. This matters more since
// db.collection.name became a metric label (ADR 0019 §7, 2026-07-29 amendment).
func TestParseStatementRejectsSplitQuotedIdentifiers(t *testing.T) {
	cases := []struct {
		name         string
		statement    string
		wantKeyspace string
		wantTable    string
	}{
		{"quoted table with space", `SELECT * FROM "message archive"`, "", ""},
		{"quoted table with space and clause", `SELECT * FROM "message archive" WHERE id = ?`, "", ""},
		{"qualified quoted table with space keeps keyspace", `SELECT * FROM chat."message archive"`, "chat", ""},
		{"quoted insert target with space", `INSERT INTO "my table" (a) VALUES (?)`, "", ""},
		{"quoted update target with space", `UPDATE "my table" SET a = ?`, "", ""},
		// Well-formed identifiers are unaffected, quoted or not.
		{"quoted single-word table", `SELECT * FROM "messages_by_room"`, "", "messages_by_room"},
		{"bare table", `SELECT * FROM messages_by_room`, "", "messages_by_room"},
		{"qualified bare table", `SELECT * FROM chat.messages_by_room`, "chat", "messages_by_room"},
		{"qualified quoted table", `SELECT * FROM chat."messages_by_room"`, "chat", "messages_by_room"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ks, tbl := parseStatement(c.statement)
			assert.Equal(t, c.wantKeyspace, ks, "keyspace")
			assert.Equal(t, c.wantTable, tbl, "table")
		})
	}
}
