package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// 1. Configuration Fuzzing: Malformed YAML and JSON
// -----------------------------------------------------------------------------

func TestAdversarial_MalformedYAMLAndJSON(t *testing.T) {
	malformedInputs := []struct {
		name  string
		input string
	}{
		{"Truncated JSON", `{"version": "1", "server": {`},
		{"Invalid JSON syntax", `{"version": 1, "server": invalid}`},
		{"YAML invalid tab indentation", "version: \"1\"\nserver:\n\ttransport: stdio"},
		{"YAML unclosed quote", "version: \"1\nserver:\n  transport: stdio"},
		{"YAML trailing garbage", "version: \"1\"\n%%INVALID_YAML_TOKEN%%"},
		{"Random binary noise", "\x00\x01\x02\xff\xfe\xfd\x80\x90"},
		{"Empty document", ""},
		{"Only whitespace", "   \n\t\n   "},
		{"Only comments", "# This is a comment\n# Another comment\n"},
		{"YAML scalar instead of map", "just a string"},
		{"YAML array instead of map", "- item1\n- item2"},
		{"YAML boolean instead of map", "true"},
		{"Null byte in YAML", "version: \"1\"\x00\nserver:\n  transport: stdio"},
	}

	for _, tc := range malformedInputs {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.input))
			if tc.name == "Empty document" || tc.name == "Only whitespace" || tc.name == "Only comments" {
				// Empty YAML may parse to default or unmarshal zero values
				return
			}
			if err == nil && cfg != nil {
				t.Logf("Parse on %s returned: %+v", tc.name, cfg)
			}
		})
	}
}

// TestAdversarial_YAMLBillionLaughs tests resilience against YAML anchor expansion bomb.
func TestAdversarial_YAMLBillionLaughs(t *testing.T) {
	// Moderately sized anchor chain to test parser expansion limits without killing process
	yamlBomb := `
a: &a ["lol","lol","lol","lol","lol","lol","lol","lol","lol"]
b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]
version: "1"
`
	done := make(chan error, 1)
	go func() {
		_, err := Parse([]byte(yamlBomb))
		done <- err
	}()

	select {
	case err := <-done:
		t.Logf("YAML anchor expansion finished (err: %v)", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("YAML parser timed out on anchor expansion (possible YAML bomb denial of service)")
	}
}

// -----------------------------------------------------------------------------
// 2. Configuration Fuzzing: Ports Fuzzing
// -----------------------------------------------------------------------------

func TestAdversarial_PortFuzzing(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		port      int
		host      string
		expectErr bool
	}{
		{"Valid SSE port 8080", "sse", 8080, "127.0.0.1", false},
		{"Valid SSE port 80", "sse", 80, "127.0.0.1", false},
		{"Valid SSE port 443", "sse", 443, "127.0.0.1", false},
		{"Valid SSE port boundary 65535", "sse", 65535, "127.0.0.1", false},
		{"SSE port negative -1", "sse", -1, "127.0.0.1", true},
		{"SSE port negative -8080", "sse", -8080, "127.0.0.1", true},
		{"SSE port overflow 65536", "sse", 65536, "127.0.0.1", true},
		{"SSE port huge overflow 999999", "sse", 999999, "127.0.0.1", true},
		{"SSE empty host", "sse", 8080, "", true},
		{"SSE whitespace host", "sse", 8080, "   ", true},
		{"Stdio with negative port (ignored for stdio)", "stdio", -1, "", false},
		{"Stdio with overflow port (ignored for stdio)", "stdio", 999999, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Server.Transport = tc.transport
			cfg.Server.Port = tc.port
			cfg.Server.Host = tc.host

			err := cfg.Validate()
			if tc.expectErr && err == nil {
				t.Errorf("expected validation error for port=%d host=%q transport=%q, but got nil", tc.port, tc.host, tc.transport)
			}
			if !tc.expectErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 3. Configuration Fuzzing: Negative and Malformed Durations
// -----------------------------------------------------------------------------

func TestAdversarial_NegativeDurations(t *testing.T) {
	// Test Duration Unmarshaling with negative durations
	negDurationJSON := `{"timeout": "-10s"}`
	var target struct {
		Timeout Duration `json:"timeout"`
	}
	err := json.Unmarshal([]byte(negDurationJSON), &target)
	if err != nil {
		t.Fatalf("json.Unmarshal failed on negative duration: %v", err)
	}
	if target.Timeout.Duration() != -10*time.Second {
		t.Fatalf("expected -10s, got %v", target.Timeout.Duration())
	}

	// Validate server.timeout cannot be negative
	cfg := DefaultConfig()
	cfg.Server.Timeout = Duration(-5 * time.Second)
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative server.timeout, got nil")
	} else if !strings.Contains(err.Error(), "server.timeout cannot be negative") {
		t.Errorf("expected 'server.timeout cannot be negative', got: %v", err)
	}

	// Challenge: What about upstream.timeout negative?
	t.Run("Upstream negative timeout audit", func(t *testing.T) {
		cfg2 := DefaultConfig()
		cfg2.Upstreams = []UpstreamConfig{
			{
				ID:        "test-up",
				Transport: "stdio",
				Command:   "echo",
				Timeout:   Duration(-10 * time.Second),
			},
		}
		err2 := cfg2.Validate()
		if err2 == nil {
			t.Logf("FINDING: UpstreamConfig.Timeout accepts negative duration (-10s) without validation error")
		} else {
			t.Logf("UpstreamConfig.Timeout rejected negative duration: %v", err2)
		}
	})
}

func TestAdversarial_MalformedDurations(t *testing.T) {
	malformed := []string{
		`"invalid"`,
		`"10x"`,
		`"5sec"`,
		`"30 minutes"`,
		`"1y"`,
		`"---5s"`,
		`"999999999999999999999999999999999999999999h"`, // integer overflow
	}

	for _, input := range malformed {
		t.Run(input, func(t *testing.T) {
			var d Duration
			err := d.UnmarshalJSON([]byte(input))
			if err == nil {
				t.Errorf("expected error for malformed duration %s, got nil (parsed as %v)", input, d.Duration())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 4. Configuration Fuzzing: Circular and Complex Environment Variables
// -----------------------------------------------------------------------------

func TestAdversarial_CircularAndComplexEnvVars(t *testing.T) {
	// 1. Circular references: A -> ${B}, B -> ${A}
	t.Setenv("TP_CIRCULAR_A", "${TP_CIRCULAR_B}")
	t.Setenv("TP_CIRCULAR_B", "${TP_CIRCULAR_A}")

	res := ExpandEnv("token: ${TP_CIRCULAR_A}")
	// ExpandEnv is single-pass, so it should expand ${TP_CIRCULAR_A} to "${TP_CIRCULAR_B}" and terminate
	if res != "token: ${TP_CIRCULAR_B}" {
		t.Errorf("expected single-pass termination 'token: ${TP_CIRCULAR_B}', got %q", res)
	}

	// 2. Self-referential: X -> ${X}
	t.Setenv("TP_SELF_REF", "${TP_SELF_REF}")
	res2 := ExpandEnv("val: ${TP_SELF_REF}")
	if res2 != "val: ${TP_SELF_REF}" {
		t.Errorf("expected single-pass termination 'val: ${TP_SELF_REF}', got %q", res2)
	}

	// 3. Deeply nested fallback syntax: ${VAR:-${OTHER:-default}}
	t.Setenv("TP_INNER", "inner_val")
	res3 := ExpandEnv("nested: ${TP_UNSET_OUTER:-${TP_INNER}}")
	t.Logf("Nested env fallback result: %q", res3)

	// 4. Huge environment variable (100KB)
	hugeVal := strings.Repeat("A", 100*1024)
	t.Setenv("TP_HUGE_VAR", hugeVal)
	res4 := ExpandEnv("huge: ${TP_HUGE_VAR}")
	if len(res4) < 100*1024 {
		t.Errorf("expected expanded length >= 100KB, got %d", len(res4))
	}

	// 5. Special chars and unclosed brackets
	unclosed := []string{
		"unclosed: ${UNCLOSED",
		"bare dollar: $",
		"dollar bracket: ${",
		"dollar empty: ${}",
		"dollar colon: ${:-default}",
		"escaped backslash: \\${VAR}",
	}
	for _, u := range unclosed {
		got := ExpandEnv(u)
		// Should not panic or hang
		if got == "" && u != "" {
			t.Errorf("ExpandEnv(%q) returned empty string unexpectedly", u)
		}
	}
}

// -----------------------------------------------------------------------------
// 5. Configuration Fuzzing: Upstream Validation Boundaries
// -----------------------------------------------------------------------------

func TestAdversarial_UpstreamValidation(t *testing.T) {
	tests := []struct {
		name        string
		modify      func(c *Config)
		expectError string
	}{
		{
			name: "Reserved double underscore in ID",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{{ID: "bad__id", Transport: "stdio", Command: "test"}}
			},
			expectError: "contains '__'",
		},
		{
			name: "Reserved double underscore in Prefix",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{{ID: "validid", Prefix: "bad__pref", Transport: "stdio", Command: "test"}}
			},
			expectError: "cannot contain '__'",
		},
		{
			name: "Duplicate upstream IDs",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{
					{ID: "dup", Transport: "stdio", Command: "test1"},
					{ID: "dup", Transport: "stdio", Command: "test2"},
				}
			},
			expectError: "duplicate upstream id",
		},
		{
			name: "Invalid upstream transport",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{{ID: "up1", Transport: "websocket", Command: "test"}}
			},
			expectError: "must be 'stdio' or 'sse'",
		},
		{
			name: "Stdio upstream with empty command",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{{ID: "up1", Transport: "stdio", Command: ""}}
			},
			expectError: "command is empty",
		},
		{
			name: "SSE upstream with empty URL",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{{ID: "up1", Transport: "sse", URL: ""}}
			},
			expectError: "url is empty",
		},
		{
			name: "SSE upstream with non-http URL",
			modify: func(c *Config) {
				c.Upstreams = []UpstreamConfig{{ID: "up1", Transport: "sse", URL: "ftp://example.com/sse"}}
			},
			expectError: "not a valid HTTP/HTTPS URL",
		},
		{
			name: "Nil config pointer to Validate()",
			modify: func(c *Config) {
				// handled in subtest
			},
			expectError: "configuration cannot be nil",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "Nil config pointer to Validate()" {
				err := Validate(nil)
				if err == nil || !strings.Contains(err.Error(), tc.expectError) {
					t.Fatalf("expected error %q, got %v", tc.expectError, err)
				}
				return
			}

			cfg := DefaultConfig()
			tc.modify(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.expectError)
			}
			if !strings.Contains(err.Error(), tc.expectError) {
				t.Fatalf("expected error containing %q, got: %v", tc.expectError, err)
			}
		})
	}
}
