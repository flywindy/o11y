package elasticsearch

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	elastic "github.com/elastic/go-elasticsearch/v8"

	"github.com/flywindy/o11y/internal/redact"
)

// envAddresses is the variable go-elasticsearch reads its addresses from when
// the Config names none.
const envAddresses = "ELASTICSEARCH_URL"

// constructorError returns the error an upstream constructor produced, in a
// form that is safe to log.
//
// go-elasticsearch accepts credentials in an address
// (https://user:pass@host:9200). When an address does not parse, the upstream
// constructor returns net/url's parse error, which quotes the address verbatim,
// and it does not parse precisely when the password holds a character that
// needed percent-encoding ("%", a space, "["). So the error appears on the
// deployments that keep the secret in the URL, and the SDK's documented logging
// pattern, slog.Any("error", err), would send it to stdout and the OTLP log
// pipeline.
//
// Three cases, each closed in its own way:
//
//   - The upstream error is an address that failed to parse. It is replaced by
//     one that says which address and why, and does not quote it. See
//     addressParseError.
//   - Anything else goes through redact.Error with the configured addresses. An
//     error that needed no redaction is returned as it is, chain included.
//   - One that did need redaction is returned as a new error carrying only the
//     redacted text — unless one of the configured addresses does not parse, in
//     which case that is what is reported. No upstream constructor error other
//     than the parse failure quotes an address today, so this is the parse
//     failure in a wording the first case did not recognise (a later
//     go-elasticsearch formatting it differently), and redacting the address
//     out of it would still leave the piece of it net/url quotes in the reason.
//
// In no case does the result unwrap to an error holding the credential.
// redact.Error on its own would: its result keeps the original as Unwrap so
// errors.Is and errors.As still work, which leaves the unredacted text one
// errors.Unwrap away from anything that renders each link of a chain. The
// upstream constructors build these errors with %s and %v, so they carry no
// chain for a caller to match on, and flattening them loses nothing.
func constructorError(err error, cfg elastic.Config) error {
	addrs, source := configuredAddresses(cfg)
	index, parseErr := firstUnparseable(addrs)
	if parseErr != nil && strings.Contains(err.Error(), parseErr.Error()) {
		return addressParseError(index, len(addrs), source, parseErr)
	}
	redacted := redact.Error(err, addrs, nil)
	if redacted.Error() == err.Error() {
		return err
	}
	if parseErr != nil {
		return addressParseError(index, len(addrs), source, parseErr)
	}
	return errors.New(redacted.Error())
}

// configuredAddresses returns the addresses the upstream constructor will
// parse and where they came from, mirroring go-elasticsearch v8.19.3
// newTransport: the Config's own list, or the comma-separated ELASTICSEARCH_URL
// when the Config names neither an address nor a CloudID.
func configuredAddresses(cfg elastic.Config) (addrs []string, source string) {
	if len(cfg.Addresses) > 0 || cfg.CloudID != "" {
		return cfg.Addresses, "cfg.Addresses"
	}
	env := os.Getenv(envAddresses)
	if env == "" {
		return nil, envAddresses
	}
	for entry := range strings.SplitSeq(env, ",") {
		addrs = append(addrs, strings.TrimSpace(entry))
	}
	return addrs, envAddresses
}

// firstUnparseable returns the position and the parse error of the first
// address the upstream constructor cannot parse, or a nil error when all of
// them parse.
//
// Each address is parsed exactly as the upstream does (addrsToURLs: url.Parse
// on the address with trailing slashes trimmed), so the *url.Error here has the
// text the upstream formats into its own message. constructorError rewrites
// the upstream error only when it contains that text: the upstream stays the
// authority on which failure is being reported, and a Config with both
// Addresses and CloudID set, which fails before any address is parsed, keeps
// saying so.
func firstUnparseable(addrs []string) (int, error) {
	for i, addr := range addrs {
		if _, err := url.Parse(strings.TrimRight(addr, "/")); err != nil {
			return i, err
		}
	}
	return 0, nil
}

// addressParseError builds the credential-free replacement for an address that
// failed to parse.
//
// It names the address by position rather than by value. An address that does
// not parse cannot be split into a host that is safe to show and a userinfo
// that is not, so none of it is shown.
func addressParseError(index, total int, source string, parseErr error) error {
	return fmt.Errorf(
		"cannot parse address %d of %d in %s: %s (the address is not shown because it may carry credentials)",
		index+1, total, source, parseReason(parseErr),
	)
}

// parseReason returns why net/url rejected an address, without the part of the
// address its error quotes.
//
// The reason is not safe to repeat as written. Three of net/url's errors quote
// a piece of the input, and for a URL with credentials that piece can come out
// of the password: an invalid escape quotes the three bytes at the "%", an
// invalid host character quotes the byte, and an invalid port quotes everything
// between the first ":" and the first "/" — which for "https://user:pa/ss@host"
// is the start of the password.
//
// So the two typed errors are named without their operand, the two formatted
// ones without theirs, and the rest are matched against the fixed texts
// net/url writes (go1.26 net/url/url.go). A
// reason that is none of those is reported as a generic one: a new wording in a
// later Go release costs the detail, not the secret.
func parseReason(parseErr error) string {
	var urlErr *url.Error
	if !errors.As(parseErr, &urlErr) || urlErr.Err == nil {
		return unknownParseReason
	}
	var escape url.EscapeError
	if errors.As(urlErr.Err, &escape) {
		return "invalid URL escape"
	}
	var host url.InvalidHostError
	if errors.As(urlErr.Err, &host) {
		return "invalid character in host name"
	}
	reason := urlErr.Err.Error()
	switch reason {
	// The net/url parse errors whose text is a constant, so repeating one
	// cannot repeat any part of the address.
	case "missing protocol scheme",
		"first path segment in URL cannot contain colon",
		"net/url: invalid control character in URL",
		"net/url: invalid userinfo",
		"missing ']' in host",
		"invalid IP-literal",
		"invalid URI for request",
		"empty url":
		return reason
	}
	switch {
	case strings.HasPrefix(reason, "invalid port ") && strings.HasSuffix(reason, " after host"):
		return "invalid port after host"
	case strings.HasPrefix(reason, "invalid host: "):
		return "invalid host"
	}
	return unknownParseReason
}

// unknownParseReason stands in for a parse failure whose text is not one this
// package can vouch for.
const unknownParseReason = "not a valid URL"
