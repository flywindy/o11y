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

// parseStatement extracts the CQL operation verb, the keyspace qualifier (when
// the addressed table is written as keyspace.table), and the single addressed
// table from a statement in one tokenization pass. ObserveQuery runs per attempt
// and per page, so sharing the strings.Fields split here avoids tokenizing the
// statement twice on every callback. The keyspace is "" unless the statement
// explicitly qualifies the table; callers use it only as a fallback for
// db.namespace when the driver reports no session keyspace.
func parseStatement(statement string) (operation, keyspace, table string) {
	fields := strings.Fields(stripComments(statement))
	if len(fields) == 0 {
		return "", "", ""
	}
	keyspace, table = parseTableFields(fields)
	return strings.ToUpper(fields[0]), keyspace, table
}

// stripComments removes CQL comments from a statement so the operation verb and
// table parse correctly around a query name, routing tag, or ORM annotation.
// CQL supports line comments (--) and block comments (/* */); the C-style // is
// not CQL and is left untouched.
//
// Comments are stripped wherever they appear, not only at the front. A comment
// sitting between the target keyword and the identifier —
// `SELECT * FROM /* routing tag */ rooms` — otherwise leaves `/*` as the token
// after FROM, and the table resolves to `/*` rather than `rooms`. That is the
// same failure mode as a split quoted identifier: a confident, wrong value, now
// carried on a metric label where a varying comment could also consume the
// collection cap (ADR 0019 §7, 2026-07-29 amendment).
//
// Quoted runs are copied verbatim so a comment marker inside a string literal or
// a quoted identifier (`WHERE name = 'a/*b'`) is not mistaken for a comment. CQL
// escapes a quote by doubling it, which falls out of this loop naturally: the
// closing quote ends one run and the next character immediately opens another.
// An unterminated comment ends the statement, since nothing after it is parseable.
func stripComments(stmt string) string {
	var b strings.Builder
	b.Grow(len(stmt))
	for i := 0; i < len(stmt); {
		switch {
		case stmt[i] == '\'' || stmt[i] == '"' || stmt[i] == '`':
			quote := stmt[i]
			b.WriteByte(stmt[i])
			for i++; i < len(stmt); i++ {
				b.WriteByte(stmt[i])
				if stmt[i] == quote {
					i++
					break
				}
			}
		case strings.HasPrefix(stmt[i:], "--"):
			nl := strings.IndexByte(stmt[i:], '\n')
			if nl < 0 {
				return b.String()
			}
			// Leave a space behind so the tokens either side do not merge into
			// one when the comment had no surrounding whitespace.
			b.WriteByte(' ')
			i += nl + 1
		case strings.HasPrefix(stmt[i:], "/*"):
			end := strings.Index(stmt[i:], "*/")
			if end < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += end + 2
		default:
			b.WriteByte(stmt[i])
			i++
		}
	}
	return b.String()
}

// parseTableFields is the shared table-parsing core operating on a pre-split
// statement, so callers that already tokenized do not split again. It returns
// the keyspace qualifier (when the addressed object is written keyspace.table,
// or for a CREATE/DROP KEYSPACE whose object is itself a keyspace) and the bare
// table name. Unrecognized shapes yield "", "".
func parseTableFields(fields []string) (keyspace, table string) {
	if len(fields) < 2 {
		return "", ""
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "DELETE":
		return afterKeyword(fields, "FROM")
	case "INSERT":
		return afterKeyword(fields, "INTO")
	case "UPDATE":
		return normalizeTable(fields[1])
	case "TRUNCATE":
		// TRUNCATE [TABLE] <name>
		if strings.ToUpper(fields[1]) == "TABLE" {
			return objectToken(fields, 2)
		}
		return objectToken(fields, 1)
	case "CREATE", "DROP", "ALTER":
		return parseDDLTarget(fields)
	default:
		return "", ""
	}
}

// afterKeyword returns the keyspace/table parsed from the token following the
// first occurrence of keyword (e.g. FROM/INTO).
func afterKeyword(fields []string, keyword string) (keyspace, table string) {
	for i := 1; i < len(fields)-1; i++ {
		if strings.ToUpper(fields[i]) == keyword {
			return normalizeTable(fields[i+1])
		}
	}
	return "", ""
}

// parseDDLTarget extracts the namespace/table from the common qualified DDL
// forms: CREATE/DROP/ALTER TABLE [IF [NOT] EXISTS] [ks.]table, and CREATE/DROP
// KEYSPACE [IF [NOT] EXISTS] ks (whose object is itself the keyspace). Other DDL
// objects (INDEX, TYPE, MATERIALIZED VIEW, …) yield "", "" rather than guessing.
func parseDDLTarget(fields []string) (keyspace, table string) {
	if len(fields) < 3 {
		return "", ""
	}
	switch strings.ToUpper(fields[1]) {
	case "TABLE", "COLUMNFAMILY":
		return objectToken(fields, 2)
	case "KEYSPACE", "SCHEMA":
		// The object names a keyspace directly; there is no table.
		_, name := objectToken(fields, 2)
		return name, ""
	default:
		return "", ""
	}
}

// objectToken returns the keyspace/table parsed from the first token at or after
// start that is not an IF/NOT/EXISTS modifier.
func objectToken(fields []string, start int) (keyspace, table string) {
	for i := start; i < len(fields); i++ {
		switch strings.ToUpper(fields[i]) {
		case "IF", "NOT", "EXISTS":
			continue
		default:
			return normalizeTable(fields[i])
		}
	}
	return "", ""
}

// normalizeTable strips trailing punctuation/clauses from a parsed table token
// and splits an explicit keyspace qualifier, returning the keyspace (or "" when
// unqualified) and the bare table name. Either half is "" when that half is not
// a complete identifier (see unquoteIdentifier).
func normalizeTable(token string) (keyspace, table string) {
	// Cut at the first character that cannot be part of an identifier so a
	// "table(col,...)" or "table;" token reduces to the table name.
	if idx := strings.IndexAny(token, "(;,"); idx >= 0 {
		token = token[:idx]
	}
	// Split a keyspace qualifier (keyspace.table) before validating, so a
	// well-formed keyspace still resolves when the table half is unusable.
	rest := token
	if idx := strings.LastIndex(token, "."); idx >= 0 {
		keyspace = unquoteIdentifier(token[:idx])
		rest = token[idx+1:]
	}
	return keyspace, unquoteIdentifier(rest)
}

// unquoteIdentifier returns the bare identifier for a single whitespace-delimited
// token, or "" when the token does not contain a complete one.
//
// CQL allows quoted identifiers containing whitespace (`SELECT * FROM "message
// archive"`). parseStatement tokenizes on whitespace, so such an identifier
// arrives here already split — the first token is `"message`, whose stray
// opening quote is the evidence that the rest was cut off. Trimming quotes
// blindly would yield `message`: a confident, wrong table name for a table that
// does not exist. An odd quote count therefore yields "" so the caller omits
// db.collection.name rather than mislabeling the operation, which matters more
// now that the value is a metric label and not only a span attribute
// (ADR 0019 §7, 2026-07-29 amendment).
func unquoteIdentifier(token string) string {
	if strings.Count(token, `"`)%2 != 0 || strings.Count(token, "`")%2 != 0 {
		return ""
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
