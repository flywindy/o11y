package cassandra

import (
	"net"
	"strconv"
	"strings"
)

// serverAddr holds the logical server (configured contact point) used for the
// server.address / server.port span attributes and metric labels. These are the
// client's configured contact points — a small, fixed set per client instance,
// not per-request peer addresses — so they are safe as metric labels without
// exploding cardinality (ADR 0019 §7).
type serverAddr struct {
	host string
	port int
}

// contactPoint derives the logical server address from the cluster config's
// contact points. gocql stores contact points in Hosts and a shared default
// Port; a host entry may itself carry a "host:port" form, which takes
// precedence over the cluster-wide Port.
func contactPoint(hosts []string, defaultPort int) serverAddr {
	if len(hosts) == 0 {
		return serverAddr{}
	}
	first := strings.TrimSpace(hosts[0])
	if first == "" {
		return serverAddr{}
	}
	if host, port, err := net.SplitHostPort(first); err == nil {
		out := serverAddr{host: host, port: defaultPort}
		if parsed, err := strconv.Atoi(port); err == nil {
			out.port = parsed
		}
		return out
	}
	return serverAddr{host: first, port: defaultPort}
}

// parseStatement extracts both the CQL operation verb and the single addressed
// table from a statement in one tokenization pass. ObserveQuery runs per attempt
// and per page, so sharing the strings.Fields split here avoids tokenizing the
// statement twice on every callback.
func parseStatement(statement string) (operation, table string) {
	fields := strings.Fields(trimLeadingComments(statement))
	if len(fields) == 0 {
		return "", ""
	}
	return strings.ToUpper(fields[0]), parseTableFields(fields)
}

// parseOperation extracts the CQL operation verb (SELECT, INSERT, UPDATE, …)
// from a statement. It returns the uppercased leading keyword, or "" when the
// statement is empty or unparseable.
func parseOperation(statement string) string {
	fields := strings.Fields(trimLeadingComments(statement))
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// trimLeadingComments strips leading whitespace and CQL comments so the
// operation verb and table parse correctly when a statement is prefixed with a
// query name, routing tag, or ORM annotation. CQL supports line comments (--)
// and block comments (/* */); the C-style // is not CQL and is left untouched.
func trimLeadingComments(stmt string) string {
	for {
		stmt = strings.TrimSpace(stmt)
		switch {
		case strings.HasPrefix(stmt, "--"):
			nl := strings.IndexByte(stmt, '\n')
			if nl < 0 {
				return ""
			}
			stmt = stmt[nl+1:]
		case strings.HasPrefix(stmt, "/*"):
			end := strings.Index(stmt, "*/")
			if end < 0 {
				return ""
			}
			stmt = stmt[end+2:]
		default:
			return stmt
		}
	}
}

// parseTable returns the single table a statement addresses, or "" when no
// single table can be confidently parsed (multi-table statements, unrecognized
// shapes). It recognizes the common single-table CQL DML forms; anything else
// falls back to "" so the span name degrades to cassandra.{operation} rather
// than guessing.
func parseTable(statement string) string {
	return parseTableFields(strings.Fields(trimLeadingComments(statement)))
}

// parseTableFields is the shared table-parsing core operating on a pre-split
// statement, so callers that already tokenized do not split again.
func parseTableFields(fields []string) string {
	if len(fields) < 2 {
		return ""
	}
	verb := strings.ToUpper(fields[0])
	switch verb {
	case "INSERT", "DELETE", "SELECT":
		// ... FROM <table> ... / INSERT INTO <table> ...
		for i := 1; i < len(fields)-1; i++ {
			kw := strings.ToUpper(fields[i])
			if kw == "FROM" || kw == "INTO" {
				return normalizeTable(fields[i+1])
			}
		}
		return ""
	case "UPDATE":
		// UPDATE <table> SET ...
		return normalizeTable(fields[1])
	default:
		return ""
	}
}

// normalizeTable strips a keyspace qualifier and trailing punctuation/clauses
// from a parsed table token, leaving the bare table name.
func normalizeTable(token string) string {
	// Cut at the first character that cannot be part of an identifier so a
	// "table(col,...)" or "table;" token reduces to the table name.
	if idx := strings.IndexAny(token, "(;,"); idx >= 0 {
		token = token[:idx]
	}
	token = strings.Trim(token, "\"`")
	// Drop a keyspace qualifier: keyspace.table -> table.
	if idx := strings.LastIndex(token, "."); idx >= 0 {
		token = token[idx+1:]
	}
	return strings.Trim(token, "\"`")
}

// spanName builds the cross-package span name (ADR 0023):
// {db.system.name}.{db.operation.name} {target}, falling back to
// cassandra.{operation} when the table cannot be parsed, and to "cassandra"
// when even the operation is unknown.
func spanName(operation, table string) string {
	switch {
	case operation == "":
		return "cassandra"
	case table == "":
		return "cassandra." + operation
	default:
		return "cassandra." + operation + " " + table
	}
}
