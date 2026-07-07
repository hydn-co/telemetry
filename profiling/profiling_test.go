package profiling

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestShouldDeriveBlobEndpointWhenTablesEndpointIsCloud(t *testing.T) {
	// Arrange
	cases := map[string]string{
		"https://acct.table.core.windows.net/":      "https://acct.blob.core.windows.net/",
		"https://acct.table.core.windows.net":       "https://acct.blob.core.windows.net",
		"https://ACCT.TABLE.CORE.WINDOWS.NET/":      "https://acct.blob.core.windows.net/",      // case-insensitive host, rewritten on host only
		"https://acct.table.core.usgovcloudapi.net": "https://acct.blob.core.usgovcloudapi.net", // sovereign
		"https://acct.table.core.chinacloudapi.cn":  "https://acct.blob.core.chinacloudapi.cn",  // sovereign
		"http://127.0.0.1:10002/devstoreaccount1":   "",                                         // Azurite path-style
		"":                                     "",
		"https://acct.queue.core.windows.net/": "", // not a tables endpoint
		"https://evil.com/?x=.table.":          "", // ".table." not in an Azure host
		"https://acct.table.core.windows.net.evil.com/": "", // look-alike host, wrong suffix
		"http://acct.table.core.windows.net/":           "", // non-https: never send tokens over plaintext
	}

	for in, want := range cases {
		// Act
		got := blobEndpointFromTables(in)

		// Assert
		if got != want {
			t.Errorf("blobEndpointFromTables(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShouldReplaceUnsafeCharsWhenSanitizingBlobSegment(t *testing.T) {
	// Arrange / Act / Assert
	if got := sanitizeBlobSegment("stream/0.2.0 RC"); got != "stream-0.2.0-RC" {
		t.Errorf("sanitizeBlobSegment = %q, want %q", got, "stream-0.2.0-RC")
	}

	if got := sanitizeBlobSegment(""); got != "unknown" {
		t.Errorf("sanitizeBlobSegment(empty) = %q, want %q", got, "unknown")
	}
}

func TestShouldUseDefaultsWhenPProfEnvUnsetOrInvalid(t *testing.T) {
	// Arrange
	const key = "MESH_PPROF_TEST_SECONDS"
	t.Setenv(key, "")

	// Act / Assert — unset falls back to default
	if got := envSeconds(key, defaultCapture); got != defaultCapture {
		t.Errorf("envSeconds(unset) = %v, want %v", got, defaultCapture)
	}

	// Invalid (non-positive / non-numeric) also falls back
	for _, bad := range []string{"0", "-5", "abc"} {
		t.Setenv(key, bad)

		if got := envSeconds(key, defaultCapture); got != defaultCapture {
			t.Errorf("envSeconds(%q) = %v, want default %v", bad, got, defaultCapture)
		}
	}

	// Valid parses to seconds
	t.Setenv(key, "120")

	if got := envSeconds(key, defaultCapture); got != 120*time.Second {
		t.Errorf("envSeconds(120) = %v, want %v", got, 120*time.Second)
	}
}

func TestShouldParseDurationWhenIntervalSet(t *testing.T) {
	// Arrange
	const key = "MESH_PPROF_TEST_INTERVAL"

	// Act / Assert
	t.Setenv(key, "90s")

	if got := envDuration(key, defaultInterval); got != 90*time.Second {
		t.Errorf("envDuration(90s) = %v, want %v", got, 90*time.Second)
	}

	t.Setenv(key, "garbage")

	if got := envDuration(key, defaultInterval); got != defaultInterval {
		t.Errorf("envDuration(garbage) = %v, want default %v", got, defaultInterval)
	}
}

func TestShouldProduceSafeNonEmptyInstanceID(t *testing.T) {
	// Arrange / Act
	id := instanceID()

	// Assert — non-empty, safe blob-segment chars, and includes the pid so it varies across processes
	if id == "" {
		t.Fatal("instanceID() is empty")
	}

	if got := sanitizeBlobSegment(id); got != id {
		t.Errorf("instanceID() = %q is not a safe blob segment (sanitized: %q)", id, got)
	}

	if !strings.Contains(id, strconv.Itoa(os.Getpid())) {
		t.Errorf("instanceID() = %q does not contain the pid %d", id, os.Getpid())
	}
}

func TestShouldReturnNoopStopWhenDisabled(t *testing.T) {
	// Arrange
	t.Setenv(envEnabled, "false")

	// Act — must not start the loop and must return a callable stop func
	stop := Start(context.Background(), "stream", "0.0.0")

	// Assert — calling stop is safe (no goroutine to wait on)
	stop()
}

func TestShouldReturnNoopStopWhenEnabledButNoBlobEndpoint(t *testing.T) {
	// Arrange — enabled but AZURE_TABLES_ENDPOINT is not a cloud tables endpoint
	t.Setenv(envEnabled, "true")
	t.Setenv(envTables, "")

	// Act
	stop := Start(context.Background(), "stream", "0.0.0")

	// Assert
	stop()
}
