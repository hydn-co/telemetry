package profiling

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
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
		"https://acct.table.core.windows.net/path":      "", // non-root path: not a bare service endpoint
		"https://acct.table.core.windows.net/?sig=x":    "", // query string (e.g. SAS): rejected
		"https://user:pw@acct.table.core.windows.net/":  "", // userinfo: rejected
		"https://acct.table.core.windows.net/#frag":     "", // fragment: rejected
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

func TestShouldValidateContainerNamesAgainstAzureRules(t *testing.T) {
	// Arrange
	valid := []string{"pprof", "a-b-1", "abc", strings.Repeat("a", 63)}
	invalid := []string{
		"ab",                    // too short (<3)
		strings.Repeat("a", 64), // too long (>63)
		"has_underscore",        // underscore not allowed
		"has space",             // space not allowed
		"-lead",                 // leading hyphen
		"trail-",                // trailing hyphen
		"a--b",                  // consecutive hyphens
		"",                      // empty
	}

	// Act / Assert
	for _, s := range valid {
		if !validContainerName(s) {
			t.Errorf("validContainerName(%q) = false, want true", s)
		}
	}

	for _, s := range invalid {
		if validContainerName(s) {
			t.Errorf("validContainerName(%q) = true, want false", s)
		}
	}
}

func TestShouldReturnNoopStopWhenDisabled(t *testing.T) {
	// Arrange — Options.Enabled is false

	// Act — must not start the loop and must return a callable stop func
	stop := Start(context.Background(), Options{Service: "stream", Version: "0.0.0"})

	// Assert — calling stop is safe (no goroutine to wait on)
	stop()
}

func TestShouldReturnNoopStopWhenEnabledButNoBlobEndpoint(t *testing.T) {
	// Arrange — enabled but AZURE_TABLES_ENDPOINT is not a cloud tables endpoint
	t.Setenv(envTables, "")

	// Act
	stop := Start(context.Background(), Options{Enabled: true, Service: "stream", Version: "0.0.0"})

	// Assert
	stop()
}
