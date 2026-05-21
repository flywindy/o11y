package o11y

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBoolEnv_Absent(t *testing.T) {
	t.Setenv("O11Y_TEST_BOOL", "")
	v, warn := parseBoolEnv("O11Y_TEST_BOOL", true)
	assert.True(t, v, "absent env var should return default")
	assert.Empty(t, warn, "absent env var should produce no warning")
}

func TestParseBoolEnv_ValidTrue(t *testing.T) {
	for _, s := range []string{"1", "t", "T", "true", "TRUE", "True"} {
		t.Setenv("O11Y_TEST_BOOL", s)
		v, warn := parseBoolEnv("O11Y_TEST_BOOL", false)
		assert.True(t, v, "value %q should parse as true", s)
		assert.Empty(t, warn)
	}
}

func TestParseBoolEnv_ValidFalse(t *testing.T) {
	for _, s := range []string{"0", "f", "F", "false", "FALSE", "False"} {
		t.Setenv("O11Y_TEST_BOOL", s)
		v, warn := parseBoolEnv("O11Y_TEST_BOOL", true)
		assert.False(t, v, "value %q should parse as false", s)
		assert.Empty(t, warn)
	}
}

func TestParseBoolEnv_InvalidValue_FallsBackToDefault(t *testing.T) {
	for _, bad := range []string{"yes", "no", "on", "off", "enabled", "2"} {
		t.Setenv("O11Y_TEST_BOOL", bad)
		v, warn := parseBoolEnv("O11Y_TEST_BOOL", true)
		assert.True(t, v, "invalid value %q should fall back to default true", bad)
		assert.NotEmpty(t, warn, "invalid value %q should produce a warning", bad)
		assert.True(t, strings.Contains(warn, "O11Y_TEST_BOOL"),
			"warning should name the env var; got: %s", warn)
		assert.True(t, strings.Contains(warn, bad),
			"warning should echo the bad value; got: %s", warn)
	}
}

func TestWithExtraHTTPServerAttributeKeys_AccumulatesAndSkipsEmpty(t *testing.T) {
	cfg := &Config{}
	WithExtraHTTPServerAttributeKeys("app_name", "")(cfg)
	WithExtraHTTPServerAttributeKeys("bot_name")(cfg)
	WithExtraHTTPServerAttributeKeys()(cfg)
	assert.Equal(t, []string{"app_name", "bot_name"}, cfg.extraHTTPServerAttrKeys)
}
