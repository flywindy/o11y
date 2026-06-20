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
		{"TRUNCATE rooms", "TRUNCATE", "", "", "cassandra.TRUNCATE"},
		{"-- routing: room-read\nSELECT id FROM messages_by_room WHERE room_id = ?", "SELECT", "", "messages_by_room", "cassandra.SELECT messages_by_room"},
		{"/* app=chat */ INSERT INTO chat.rooms (id) VALUES (?)", "INSERT", "chat", "rooms", "cassandra.INSERT rooms"},
		{"", "", "", "", "cassandra"},
	}
	for _, c := range cases {
		t.Run(c.statement, func(t *testing.T) {
			op := parseOperation(c.statement)
			tbl := parseTable(c.statement)
			assert.Equal(t, c.operation, op)
			assert.Equal(t, c.table, tbl)
			assert.Equal(t, c.span, spanName(op, tbl))

			gotOp, gotKs, gotTbl := parseStatement(c.statement)
			assert.Equal(t, c.operation, gotOp)
			assert.Equal(t, c.keyspace, gotKs, "parsed keyspace qualifier")
			assert.Equal(t, c.table, gotTbl)
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
