package mongo

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// fakeMongod is an in-process wire-protocol server that answers hello as a
// writable primary and acknowledges every other command. It exists so this
// package can drive the real driver — and therefore observe the real shape of
// event.CommandStartedEvent.ConnectionID — without a container.
type fakeMongod struct {
	ln net.Listener

	// hold delays replies to one command so several connections are in flight
	// at once. See holdCommand.
	hold *commandBarrier
}

// commandBarrier holds replies to one command until count of them are in
// flight, then releases them together.
type commandBarrier struct {
	command string
	count   int

	mu      sync.Mutex
	waiting int
	release chan struct{}
	opened  sync.Once
}

// barrierTimeout releases a barrier that never fills, so a wrong assumption
// about pool behaviour fails the test's own assertion instead of hanging it.
const barrierTimeout = 10 * time.Second

// holdCommand makes the fake delay its reply to command until count of them
// are in flight at once, then answer them together.
//
// This is what makes pool growth deterministic. The driver opens a new
// connection only when every pooled one is busy, so a fake that answers
// immediately lets a single connection serve concurrent commands in turn: how
// many connections a test actually opens is then a function of goroutine
// scheduling, and an assertion on that count is a latent flake. Holding the
// replies keeps each connection checked out until the barrier opens, so the
// pool has to grow to count connections.
//
// Call it before the first command. It does not hold the handshake, which each
// new connection must complete before it can carry the held command.
func (f *fakeMongod) holdCommand(command string, count int) {
	f.hold = &commandBarrier{
		command: command,
		count:   count,
		release: make(chan struct{}),
	}
}

// wait blocks until the barrier for command opens. A command the barrier does
// not name passes straight through.
func (b *commandBarrier) wait(command string) {
	if b == nil || b.count <= 0 || command != b.command {
		return
	}

	b.mu.Lock()
	b.waiting++
	reached := b.waiting >= b.count
	b.mu.Unlock()
	if reached {
		b.open()
		return
	}

	select {
	case <-b.release:
	case <-time.After(barrierTimeout):
		b.open()
	}
}

// open releases every waiter, once.
func (b *commandBarrier) open() {
	b.opened.Do(func() { close(b.release) })
}

// startFakeMongod listens on a loopback port and serves until the test ends.
func startFakeMongod(t *testing.T) *fakeMongod {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeMongod{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

// uri is the connection string for this fake.
func (f *fakeMongod) uri() string { return "mongodb://" + f.ln.Addr().String() }

// hostPort is the address the fake actually listens on, which is what the peer
// attributes must resolve to.
func (f *fakeMongod) hostPort() (string, int) {
	addr := f.ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// commandName returns the command a wire message carries: the first key of the
// command document, unwrapping the legacy $query envelope.
func commandName(cmd bsoncore.Document) string {
	elems, _ := cmd.Elements()
	if len(elems) == 0 {
		return ""
	}
	key := elems[0].Key()
	if key == "$query" {
		if sub, ok := elems[0].Value().DocumentOK(); ok {
			return commandName(sub)
		}
	}
	return key
}

// respond answers hello as a writable primary and acknowledges everything else.
func (f *fakeMongod) respond(cmd bsoncore.Document) bson.D {
	switch commandName(cmd) {
	case "isMaster", "ismaster", "hello":
		return bson.D{
			{Key: "ismaster", Value: true}, {Key: "isWritablePrimary", Value: true}, {Key: "helloOk", Value: true},
			{Key: "maxBsonObjectSize", Value: int32(16777216)}, {Key: "maxMessageSizeBytes", Value: int32(48000000)},
			{Key: "maxWriteBatchSize", Value: int32(100000)}, {Key: "localTime", Value: bson.NewDateTimeFromTime(time.Now())},
			{Key: "logicalSessionTimeoutMinutes", Value: int32(30)}, {Key: "connectionId", Value: int32(1)},
			{Key: "minWireVersion", Value: int32(0)}, {Key: "maxWireVersion", Value: int32(21)},
			{Key: "readOnly", Value: false}, {Key: "ok", Value: 1.0},
		}
	default:
		return bson.D{{Key: "ok", Value: 1.0}}
	}
}

// serve answers one connection's wire messages until it closes.
func (f *fakeMongod) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var nextID uint32 = 1000
	for {
		hdr := make([]byte, 16)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		length := binary.LittleEndian.Uint32(hdr[0:4])
		reqID := binary.LittleEndian.Uint32(hdr[4:8])
		opcode := binary.LittleEndian.Uint32(hdr[12:16])
		if length < 16 || length > maxWireMessage {
			return
		}
		body := make([]byte, length-16)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}

		var cmd bsoncore.Document
		switch opcode {
		case opQuery: // flags(4) cstring ns skip(4) limit(4) doc
			p := 4
			for p < len(body) && body[p] != 0 {
				p++
			}
			p += 1 + 4 + 4
			if p > len(body) {
				return
			}
			cmd = bsoncore.Document(body[p:])
		case opMsg: // flags(4) section kind(1) doc
			if len(body) < 5 {
				return
			}
			cmd = bsoncore.Document(body[5:])
		default:
			return
		}

		f.hold.wait(commandName(cmd))

		doc, _ := bson.Marshal(f.respond(cmd))
		nextID++
		var out []byte
		switch opcode {
		case opQuery:
			out = header(out, wireLen(16+4+8+4+4+len(doc)), nextID, reqID, opReply)
			out = binary.LittleEndian.AppendUint32(out, 0) // responseFlags
			out = binary.LittleEndian.AppendUint64(out, 0) // cursorID
			out = binary.LittleEndian.AppendUint32(out, 0) // startingFrom
			out = binary.LittleEndian.AppendUint32(out, 1) // numberReturned
			out = append(out, doc...)
		case opMsg:
			out = header(out, wireLen(16+4+1+len(doc)), nextID, reqID, opMsg)
			out = binary.LittleEndian.AppendUint32(out, 0) // flagBits
			out = append(out, 0)                           // section kind 0
			out = append(out, doc...)
		}
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

const (
	opReply = 1
	opQuery = 2004
	opMsg   = 2013

	// maxWireMessage caps a message the fake will read; real traffic here is a
	// few hundred bytes.
	maxWireMessage = 1 << 20
)

// wireLen narrows a reply length to the header's uint32 field; a panic beats a
// truncated frame.
func wireLen(n int) uint32 {
	if n < 0 || n > maxWireMessage {
		panic("fakeMongod: reply length out of range")
	}
	// #nosec G115 -- bounds checked directly above
	return uint32(n)
}

// header appends a 16-byte wire-protocol message header to dst.
func header(dst []byte, length, reqID, respTo, opcode uint32) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, length)
	dst = binary.LittleEndian.AppendUint32(dst, reqID)
	dst = binary.LittleEndian.AppendUint32(dst, respTo)
	dst = binary.LittleEndian.AppendUint32(dst, opcode)
	return dst
}
