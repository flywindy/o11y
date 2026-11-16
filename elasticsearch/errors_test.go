package elasticsearch

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	elastic "github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// constructors runs a check against both entry points, which share the
// redaction and must not drift apart.
var constructors = []struct {
	name   string
	prefix string
	build  func(elastic.Config) error
}{
	{
		name:   "NewClient",
		prefix: "elasticsearch new client: ",
		build: func(cfg elastic.Config) error {
			_, err := NewClient(cfg, noop.NewTracerProvider(), noopMeter())
			return err
		},
	},
	{
		name:   "NewTypedClient",
		prefix: "elasticsearch new typed client: ",
		build: func(cfg elastic.Config) error {
			_, err := NewTypedClient(cfg, noop.NewTracerProvider(), noopMeter())
			return err
		},
	},
}

// chainTexts returns the message of err and of every error reachable from it
// through Unwrap, single or multiple. It is what an error reporter that renders
// each link of a chain would print.
func chainTexts(err error) []string {
	var texts []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		texts = append(texts, e.Error())
		switch u := e.(type) { //nolint:errorlint // walking the chain link by link is the point
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return texts
}

// TestConstructors_DoNotEchoAMalformedAddress pins that an address which fails
// to parse is not quoted back in the constructor's error.
//
// go-elasticsearch accepts credentials in an address and returns net/url's
// parse error for one it cannot parse, which quotes the address verbatim. The
// address fails to parse exactly when the password holds a character that
// needed percent-encoding, so the error appears on the deployments that keep
// the secret in the URL.
//
// The reason is checked as well as the address. net/url repeats a piece of the
// input in three of its errors, and for these addresses that piece comes out of
// the password: "%ZZ" for the invalid escape, ":pa" for the invalid port.
func TestConstructors_DoNotEchoAMalformedAddress(t *testing.T) {
	tests := []struct {
		name      string
		addresses []string
		forbidden []string
		want      string
	}{
		{
			name: "an unescaped percent sign in the password",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			addresses: []string{"https://esadmin:s3cr%ZZetpw@es.internal:9200"},
			forbidden: []string{"esadmin", "s3cr", "%ZZ", "etpw", "es.internal"},
			want:      "cannot parse address 1 of 1 in cfg.Addresses: invalid URL escape",
		},
		{
			name: "a space in the password and an unclosed bracket",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			addresses: []string{"https://esadmin:pa ss@[bad"},
			forbidden: []string{"esadmin", "pa ss", "[bad"},
			want:      "cannot parse address 1 of 1 in cfg.Addresses: missing ']' in host",
		},
		{
			name: "an unescaped slash in the password, which net/url reports as a port",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			addresses: []string{"https://esadmin:hunter2/tail@es.internal:9200"},
			forbidden: []string{"esadmin", "hunter2", "tail", "es.internal"},
			want:      "cannot parse address 1 of 1 in cfg.Addresses: invalid port after host",
		},
		{
			name: "an unescaped hash in the password, where net/url quotes a truncated address",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			addresses: []string{"https://esadmin:hunter2#tail@es.internal:9200"},
			forbidden: []string{"esadmin", "hunter2", "tail", "es.internal"},
			want:      "cannot parse address 1 of 1 in cfg.Addresses: invalid port after host",
		},
		{
			name: "a space in the host",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			addresses: []string{"https://esadmin:hunter2@es internal:9200"},
			forbidden: []string{"esadmin", "hunter2", "es internal"},
			want:      "cannot parse address 1 of 1 in cfg.Addresses: invalid character in host name",
		},
		{
			name: "a bracketed host that is not an IP address",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			addresses: []string{"https://esadmin:hunter2@[nothex]:9200"},
			forbidden: []string{"esadmin", "hunter2", "nothex"},
			want:      "cannot parse address 1 of 1 in cfg.Addresses: invalid host (",
		},
		{
			name: "the second of three addresses, so the position has to say which",
			addresses: []string{
				"https://es-1.internal:9200",
				// #nosec G101 -- fabricated fixture URL, not a live credential
				// nosemgrep: gosec.G101-1
				"https://esadmin:s3cr%ZZetpw@es-2.internal:9200",
				"https://es-3.internal:9200",
			},
			forbidden: []string{"esadmin", "s3cr", "%ZZ", "etpw", "es-2.internal"},
			want:      "cannot parse address 2 of 3 in cfg.Addresses: invalid URL escape",
		},
	}
	for _, tt := range tests {
		for _, ctor := range constructors {
			t.Run(tt.name+"/"+ctor.name, func(t *testing.T) {
				_, upstream := elastic.NewClient(elastic.Config{Addresses: tt.addresses})
				require.Error(t, upstream, "the premise: the upstream constructor rejects this address")
				require.Contains(t, upstream.Error(), tt.forbidden[0],
					"and its error quotes the address, which is what this facade must not pass on")

				err := ctor.build(elastic.Config{Addresses: tt.addresses})
				require.Error(t, err)

				assert.True(t, strings.HasPrefix(err.Error(), ctor.prefix), "got %q", err)
				assert.Contains(t, err.Error(), tt.want, "the position and the reason are kept")
				for _, text := range chainTexts(err) {
					for _, secret := range tt.forbidden {
						assert.NotContains(t, text, secret,
							"no link of the chain may carry the address, so unwrapping is as safe as printing")
					}
				}
			})
		}
	}
}

// TestConstructors_DoNotEchoAMalformedEnvironmentAddress pins the same for the
// addresses go-elasticsearch reads from ELASTICSEARCH_URL when the Config names
// none, and that the message says the variable is where to look.
func TestConstructors_DoNotEchoAMalformedEnvironmentAddress(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	t.Setenv("ELASTICSEARCH_URL", "https://es-1.internal:9200, https://esadmin:s3cr%ZZetpw@es-2.internal:9200")

	for _, ctor := range constructors {
		t.Run(ctor.name, func(t *testing.T) {
			err := ctor.build(elastic.Config{})
			require.Error(t, err)

			assert.Contains(t, err.Error(), "cannot parse address 2 of 2 in ELASTICSEARCH_URL: invalid URL escape")
			for _, text := range chainTexts(err) {
				for _, secret := range []string{"esadmin", "s3cr", "%ZZ", "etpw", "es-2.internal"} {
					assert.NotContains(t, text, secret)
				}
			}
		})
	}
}

// TestPinAddresses pins which list the upstream constructor is given and where
// it is said to come from, mirroring go-elasticsearch's own precedence: the
// Config's addresses, then nothing when a CloudID is set, then the variable.
func TestPinAddresses(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		cfg        elastic.Config
		wantAddrs  []string
		wantSource string
	}{
		{
			name:       "the Config's addresses win and the variable is not read",
			env:        "https://from-env:9200",
			cfg:        elastic.Config{Addresses: []string{"https://from-cfg:9200"}},
			wantAddrs:  []string{"https://from-cfg:9200"},
			wantSource: "cfg.Addresses",
		},
		{
			name:       "a CloudID means the variable is not read either",
			env:        "https://from-env:9200",
			cfg:        elastic.Config{CloudID: "name:Zm9v"},
			wantAddrs:  nil,
			wantSource: "cfg.Addresses",
		},
		{
			name:       "the variable is split on commas and trimmed, empty entries kept, as upstream does",
			env:        " https://es-1:9200 ,,https://es-2:9200/",
			wantAddrs:  []string{"https://es-1:9200", "", "https://es-2:9200/"},
			wantSource: "ELASTICSEARCH_URL",
		},
		{
			name:       "an unset variable pins the upstream default, so the upstream has no reason to read it later",
			env:        "",
			wantAddrs:  []string{"http://localhost:9200"},
			wantSource: "the default address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ELASTICSEARCH_URL", tt.env)

			got, source := pinAddresses(tt.cfg)

			assert.Equal(t, tt.wantAddrs, got.Addresses)
			assert.Equal(t, tt.wantSource, source)
		})
	}
}

// TestConstructors_ReadTheEnvironmentOnce pins that the address list is settled
// before the upstream constructor runs, so the list that is parsed and the list
// the error is redacted against cannot be two different readings of
// ELASTICSEARCH_URL.
//
// The window this closes: the upstream reads the variable, fails on an address
// with credentials in it, and by the time the facade reads the variable to
// redact the error it holds something else — so no address is found at fault
// and the upstream error goes out as it is. The test stands in for the other
// goroutine by changing the variable between the two steps.
func TestConstructors_ReadTheEnvironmentOnce(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	t.Setenv("ELASTICSEARCH_URL", "https://esadmin:hunter2#tail@es.internal:9200")
	pinned, source := pinAddresses(elastic.Config{})

	// From here on the variable says something else.
	t.Setenv("ELASTICSEARCH_URL", "https://replaced.internal:9200")

	_, upstream := elastic.NewClient(pinned)
	require.Error(t, upstream, "the upstream parses the pinned list, not the variable's new value")
	require.Contains(t, upstream.Error(), "hunter2", "the premise: its error quotes the credential")

	got := constructorError(upstream, pinned.Addresses, source)

	assert.Equal(t,
		"cannot parse address 1 of 1 in ELASTICSEARCH_URL: invalid port after host "+
			"(the address is not shown because it may carry credentials)",
		got.Error())
	for _, text := range chainTexts(got) {
		assert.NotContains(t, text, "hunter2")
		assert.NotContains(t, text, "esadmin")
	}
}

// TestConstructors_DoNotReadTheEnvironmentAfterPinning pins the other window:
// the variable is unset when the facade settles the address list and is set, to
// an address with credentials that does not parse, before the upstream
// constructor runs.
//
// Had the facade left an unset variable for the upstream to deal with, the
// upstream would read the new value, fail on it, and return an error quoting
// the password — "hunter2" here, in a parse error net/url truncates at the "#",
// so there is no "@" left for the generic redaction to hold on to — while the
// facade, having pinned nothing, had no address to recognise it by. With the
// default pinned the upstream does not read the variable at all, so there is no
// such error to redact. Both upstream constructors are checked, since they
// share the address handling but are separate entry points.
func TestConstructors_DoNotReadTheEnvironmentAfterPinning(t *testing.T) {
	upstreams := map[string]func(elastic.Config) (interface{ URLs() []*url.URL }, error){
		"NewClient": func(cfg elastic.Config) (interface{ URLs() []*url.URL }, error) {
			client, err := elastic.NewClient(cfg)
			if err != nil {
				return nil, err
			}
			return client.Transport.(*elastictransport.Client), nil
		},
		"NewTypedClient": func(cfg elastic.Config) (interface{ URLs() []*url.URL }, error) {
			client, err := elastic.NewTypedClient(cfg)
			if err != nil {
				return nil, err
			}
			return client.Transport.(*elastictransport.Client), nil
		},
	}
	for name, construct := range upstreams {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ELASTICSEARCH_URL", "")
			pinned, source := pinAddresses(elastic.Config{})

			// From here on the variable holds a credential that does not parse.
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			t.Setenv("ELASTICSEARCH_URL", "https://esadmin:hunter2#tail@es.internal:9200")

			// The premise: an upstream left to read the variable fails on it
			// and quotes the credential.
			_, unpinned := construct(elastic.Config{})
			require.Error(t, unpinned)
			require.Contains(t, unpinned.Error(), "hunter2")

			transport, err := construct(pinned)
			require.NoError(t, err, "the pinned Config gives the upstream nothing to read the variable for")
			require.Len(t, transport.URLs(), 1)
			assert.Equal(t, "http://localhost:9200", transport.URLs()[0].String())

			// And were the upstream to fail for another reason, the list the
			// error is checked against is the one it was given.
			assert.Equal(t, []string{"http://localhost:9200"}, pinned.Addresses)
			assert.Equal(t, "the default address", source)
		})
	}
}

// TestPinAddresses_DefaultIsTheUpstreamDefault pins that the address written in
// for an unconfigured client is the one go-elasticsearch would have chosen
// itself. The upstream constant is unexported, so the facade carries a copy;
// this is what keeps the copy honest across an upgrade.
func TestPinAddresses_DefaultIsTheUpstreamDefault(t *testing.T) {
	t.Setenv("ELASTICSEARCH_URL", "")

	client, err := elastic.NewClient(elastic.Config{})
	require.NoError(t, err)
	urls := client.Transport.(*elastictransport.Client).URLs()
	require.Len(t, urls, 1, "the premise: an unconfigured upstream client has exactly one address")

	pinned, _ := pinAddresses(elastic.Config{})

	assert.Equal(t, []string{urls[0].String()}, pinned.Addresses)
}

// TestPinAddresses_DoesNotWriteThroughTheCallersConfig pins that the caller's
// Config is left as it was: the constructors take it by value and say so.
func TestPinAddresses_DoesNotWriteThroughTheCallersConfig(t *testing.T) {
	t.Setenv("ELASTICSEARCH_URL", "https://es-1:9200")
	original := elastic.Config{}

	pinned, _ := pinAddresses(original)

	assert.Empty(t, original.Addresses)
	assert.Equal(t, []string{"https://es-1:9200"}, pinned.Addresses)
}

// TestConstructors_KeepOtherUpstreamErrors pins that only the address parse
// failure is rewritten. A Config with both Addresses and CloudID fails before
// any address is parsed, so that is what the error has to keep saying even
// when one of the addresses would not have parsed either.
func TestConstructors_KeepOtherUpstreamErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  elastic.Config
		want string
	}{
		{
			name: "both Addresses and CloudID",
			cfg: elastic.Config{
				CloudID: "name:Zm9v",
				// #nosec G101 -- fabricated fixture URL, not a live credential
				// nosemgrep: gosec.G101-1
				Addresses: []string{"https://esadmin:s3cr%ZZetpw@es.internal:9200"},
			},
			want: "cannot create client: both Addresses and CloudID are set",
		},
		{
			name: "a CloudID that is not one",
			cfg:  elastic.Config{CloudID: "not-a-cloud-id"},
			want: `cannot create client: cannot parse CloudID: unexpected format: "not-a-cloud-id"`,
		},
	}
	for _, tt := range tests {
		for _, ctor := range constructors {
			t.Run(tt.name+"/"+ctor.name, func(t *testing.T) {
				err := ctor.build(tt.cfg)
				require.Error(t, err)

				assert.Equal(t, ctor.prefix+tt.want, err.Error())
				assert.NotContains(t, err.Error(), "esadmin")
			})
		}
	}
}

// TestConstructorError_DoesNotUnwrapToACredential pins the rule for an upstream
// error this package does not recognise: when its text had to be redacted, the
// result is a new error, not a wrapper around the original.
//
// redact.Error keeps the original as Unwrap so errors.Is and errors.As still
// work, which is right for Shutdown and wrong here: the upstream constructors
// return plain text, so the chain has nothing to match on and would only put
// the credential one errors.Unwrap away.
func TestConstructorError_DoesNotUnwrapToACredential(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	const address = "https://esadmin:hunter2@es.internal:9200"
	upstream := fmt.Errorf("error creating transport: %w",
		&url.Error{Op: "Get", URL: address, Err: errors.New("connection refused")})

	got := constructorError(upstream, []string{address}, "cfg.Addresses")

	for _, text := range chainTexts(got) {
		assert.NotContains(t, text, "esadmin")
		assert.NotContains(t, text, "hunter2")
	}
	assert.Contains(t, got.Error(), "connection refused", "the cause is still readable")
	assert.Contains(t, got.Error(), "es.internal:9200", "and so is the server, which is not a secret")
	assert.NotErrorIs(t, got, upstream, "the unredacted error is not reachable from the result")
}

// TestConstructorError_ReportsTheParseFailureInAWordingItDoesNotRecognise pins
// what happens if a later go-elasticsearch formats the parse failure
// differently, so the upstream text no longer contains net/url's error as this
// package reproduces it.
//
// Redacting the address out of such a message is not enough: the reason still
// quotes the start of the password. An upstream error that quoted a configured
// address while one of them does not parse is reported as that parse failure.
func TestConstructorError_ReportsTheParseFailureInAWordingItDoesNotRecognise(t *testing.T) {
	// #nosec G101 -- fabricated fixture URL, not a live credential
	// nosemgrep: gosec.G101-1
	const address = "https://esadmin:hunter2/tail@es.internal:9200"
	upstream := errors.New(`cannot create client: bad address ` + address + `: invalid port ":hunter2" after host`)

	got := constructorError(upstream, []string{address}, "cfg.Addresses")

	assert.Equal(t,
		"cannot parse address 1 of 1 in cfg.Addresses: invalid port after host "+
			"(the address is not shown because it may carry credentials)",
		got.Error())
	assert.NotErrorIs(t, got, upstream)
}

// TestConstructorError_KeepsAnErrorThatNeededNoRedaction pins the other half:
// an error with nothing to remove comes back as it was, chain included, so a
// future upstream sentinel stays matchable.
func TestConstructorError_KeepsAnErrorThatNeededNoRedaction(t *testing.T) {
	sentinel := errors.New("unsupported option")
	upstream := fmt.Errorf("error creating transport: %w", sentinel)

	got := constructorError(upstream, []string{"https://es.internal:9200"}, "cfg.Addresses")

	assert.Equal(t, upstream, got)
	assert.ErrorIs(t, got, sentinel)
}

// TestParseReason pins what is kept of each net/url parse failure. The errors
// are produced by url.Parse rather than written out, so a Go release that
// rewords one shows up here as the generic reason instead of going unnoticed.
// The inputs carry no credentials: the classification does not depend on any,
// and the constructor tests above cover the addresses that do.
func TestParseReason(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "https://h/%ZZ", want: "invalid URL escape"},
		{raw: "https://h h", want: "invalid character in host name"},
		{raw: "https://h:x", want: "invalid port after host"},
		{raw: "https://[zz]", want: "invalid host"},
		{raw: "https://[::1", want: "missing ']' in host"},
		{raw: "https://u u@h", want: "net/url: invalid userinfo"},
		{raw: "https://h/\x7f", want: "net/url: invalid control character in URL"},
		{raw: "1:2", want: "first path segment in URL cannot contain colon"},
		{raw: "://h", want: "missing protocol scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			_, err := url.Parse(tt.raw)
			require.Error(t, err, "the premise: net/url rejects %q", tt.raw)

			assert.Equal(t, tt.want, parseReason(err))
		})
	}

	t.Run("a reason this package cannot vouch for is not repeated", func(t *testing.T) {
		unknown := &url.Error{Op: "parse", URL: "x", Err: errors.New(`new wording quoting "hunter2"`)}

		assert.Equal(t, "not a valid URL", parseReason(unknown))
		assert.Equal(t, "not a valid URL", parseReason(errors.New("not a url.Error at all")))
	})
}
