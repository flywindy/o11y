package redact

import (
	"testing"
	"unicode/utf8"
)

// The escape decoders bound every conversion they make; these cases pin what
// they decode to, including the values past the last rune that an eight-digit
// \U escape can spell, which must come out as RuneError as before.
func TestGoUnescaped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`\x41`, "A"},
		{`\xff`, "\xff"},
		{`é`, "é"},
		{`\U0001F600`, "😀"},
		{`\U0010FFFF`, string(rune(utf8.MaxRune))},
		{`\U00110000`, string(utf8.RuneError)},
		{`\UFFFFFFFF`, string(utf8.RuneError)},
		{`\uD800`, string(utf8.RuneError)},
		{`a\nb`, "a\nb"},
		{`\xZZ`, `\xZZ`},
	} {
		if got := goUnescaped(tc.in); got != tc.want {
			t.Errorf("goUnescaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJSONUnescaped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`A`, "A"},
		{`￿`, "￿"},
		{`\uD800`, string(utf8.RuneError)},
		{`\uZZZZ`, `\uZZZZ`},
	} {
		if got := jsonUnescaped(tc.in); got != tc.want {
			t.Errorf("jsonUnescaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
