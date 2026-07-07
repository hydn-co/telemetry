// Package profiling provides a periodic runtime/pprof capture-to-blob loop for services that have no
// reachable /debug/pprof endpoint (no ingress, container exec blocked) but do write state to an Azure
// storage account. It captures CPU + heap profiles and uploads them to that account's blob service,
// authenticated with the service's managed identity.
//
// It is a separate subpackage on purpose: only importers of this package pull in the Azure
// blob/identity SDKs, so services that only need OTLP telemetry keep a lean dependency graph.
//
// Configuration is entirely from environment variables (shared across consumers so ops knowledge
// transfers):
//
//   - MESH_PPROF_ENABLED   — "true"/"1" turns the loop on. Anything else is a no-op.
//   - MESH_PPROF_SECONDS   — CPU capture window per cycle, in seconds (default 30).
//   - MESH_PPROF_INTERVAL  — sleep between cycles, as a Go duration (default 5m).
//   - MESH_PPROF_CONTAINER — blob container for uploads (default "pprof").
//   - AZURE_TABLES_ENDPOINT — the blob endpoint is derived from this by swapping ".table." -> ".blob.".
//   - AZURE_CLIENT_ID      — user-assigned managed identity to authenticate with (falls back to
//     system-assigned when unset).
//
// The managed identity needs Storage Blob Data Contributor on the account. Every error is logged and the
// loop continues — profiling must never crash the service.
package profiling

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	runtimepprof "runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

const (
	envEnabled   = "MESH_PPROF_ENABLED"
	envSecondsK  = "MESH_PPROF_SECONDS"
	envIntervalK = "MESH_PPROF_INTERVAL"
	envContainer = "MESH_PPROF_CONTAINER"
	envTables    = "AZURE_TABLES_ENDPOINT"
	envClientID  = "AZURE_CLIENT_ID"

	defaultCapture   = 30 * time.Second
	defaultInterval  = 5 * time.Minute
	defaultContainer = "pprof"

	// blobOpTimeout bounds each best-effort blob operation (container ensure, profile upload) so a
	// stalled network/MSI path can't hang the capture goroutine or delay shutdown.
	blobOpTimeout = 60 * time.Second
)

// Start launches the capture loop when MESH_PPROF_ENABLED is set, uploading CPU + heap profiles to blob
// storage under <service>/<version>/. It returns a stop func (a no-op when disabled or misconfigured, so
// callers can always defer it).
//
// Call Start at most once per process: CPU profiling is process-global (runtime/pprof allows only one
// active CPU profile), so a second concurrent loop would fail every StartCPUProfile and still upload
// redundant heap profiles.
func Start(ctx context.Context, service, version string) func() {
	noop := func() {}

	if !enabled() {
		return noop
	}

	endpoint := blobEndpointFromTables(os.Getenv(envTables))
	if endpoint == "" {
		slog.WarnContext(ctx,
			"pprof enabled but no blob endpoint could be derived from AZURE_TABLES_ENDPOINT; pprof not started",
			slog.String("service", service))

		return noop
	}

	cred, err := credential()
	if err != nil {
		slog.ErrorContext(ctx, "pprof: failed to create azure credential", slog.String("error", err.Error()))

		return noop
	}

	client, err := azblob.NewClient(endpoint, cred, nil)
	if err != nil {
		slog.ErrorContext(ctx, "pprof: failed to create blob client", slog.String("error", err.Error()))

		return noop
	}

	// Azure blob container names must be lowercase; normalize so a mixed-case override doesn't fail
	// CreateContainer/UploadBuffer every cycle.
	container := strings.ToLower(envOrDefaultStr(envContainer, defaultContainer))
	capture := envSeconds(envSecondsK, defaultCapture)
	interval := envDuration(envIntervalK, defaultInterval)

	loopCtx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		slog.InfoContext(loopCtx, "pprof capture-to-blob started",
			slog.String("service", service), slog.String("endpoint", endpoint),
			slog.String("container", container), slog.Duration("capture", capture),
			slog.Duration("interval", interval))

		// Best-effort container ensure, in the goroutine (not Start) so a stalled MSI/network path never
		// delays the caller's startup; an already-existing container is not an error. Bounded so it can't
		// hang the first cycle.
		ensureCtx, ensureCancel := context.WithTimeout(loopCtx, blobOpTimeout)
		if _, err := client.CreateContainer(ensureCtx, container, nil); err != nil &&
			!bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			slog.WarnContext(loopCtx, "pprof: could not ensure blob container (will still attempt uploads)",
				slog.String("container", container), slog.String("error", err.Error()))
		}
		ensureCancel()

		for {
			captureCycle(loopCtx, client, container, service, version, capture)

			select {
			case <-loopCtx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	return func() {
		cancel()
		wg.Wait()
	}
}

// captureCycle captures one CPU profile (over captureDur) plus a heap snapshot and uploads both.
// It recovers from any panic so a bad capture never takes down the service.
func captureCycle(
	ctx context.Context,
	client *azblob.Client,
	container, service, version string,
	captureDur time.Duration,
) {
	defer func() {
		if r := recover(); r != nil {
			// A panic between StartCPUProfile and StopCPUProfile would leave the process-global CPU profile
			// running, so every subsequent cycle's StartCPUProfile would fail. StopCPUProfile is a safe
			// no-op when none is active, so call it unconditionally on the panic path.
			runtimepprof.StopCPUProfile()
			slog.ErrorContext(ctx, "pprof: capture cycle panicked", slog.Any("recover", r))
		}
	}()

	// instance disambiguates profiles captured in the same second by different replicas or restarts of
	// the same service+version, so concurrent uploads to the same prefix don't overwrite each other.
	prefix := fmt.Sprintf("%s/%s", sanitizeBlobSegment(service), sanitizeBlobSegment(version))
	ts := time.Now().UTC().Format("20060102T150405Z")
	instance := instanceID()

	var cpu bytes.Buffer
	if err := runtimepprof.StartCPUProfile(&cpu); err != nil {
		slog.ErrorContext(ctx, "pprof: StartCPUProfile failed", slog.String("error", err.Error()))
	} else {
		select {
		case <-ctx.Done():
			runtimepprof.StopCPUProfile()

			return
		case <-time.After(captureDur):
		}

		runtimepprof.StopCPUProfile()
		upload(ctx, client, container, fmt.Sprintf("%s/cpu-%s-%s.pprof", prefix, ts, instance), cpu.Bytes())
	}

	var heap bytes.Buffer
	if err := runtimepprof.Lookup("heap").WriteTo(&heap, 0); err != nil {
		slog.ErrorContext(ctx, "pprof: heap profile failed", slog.String("error", err.Error()))

		return
	}

	upload(ctx, client, container, fmt.Sprintf("%s/heap-%s-%s.pprof", prefix, ts, instance), heap.Bytes())
}

func upload(ctx context.Context, client *azblob.Client, container, blobName string, data []byte) {
	if len(data) == 0 {
		return
	}

	// Bound the upload so a stalled network can't hang the capture goroutine (still respects parent
	// cancellation on shutdown).
	ctx, cancel := context.WithTimeout(ctx, blobOpTimeout)
	defer cancel()

	if _, err := client.UploadBuffer(ctx, container, blobName, data, nil); err != nil {
		slog.ErrorContext(ctx, "pprof: upload failed",
			slog.String("blob", blobName), slog.String("error", err.Error()))

		return
	}

	// Debug, not Info: this fires twice per cycle per replica; Info is reserved for lifecycle events.
	slog.DebugContext(ctx, "pprof: uploaded profile", slog.String("blob", blobName), slog.Int("bytes", len(data)))
}

func enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envEnabled))) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// credential builds a managed-identity credential, honoring AZURE_CLIENT_ID for a user-assigned identity
// and falling back to system-assigned otherwise.
func credential() (azcore.TokenCredential, error) {
	if clientID := strings.TrimSpace(os.Getenv(envClientID)); clientID != "" {
		return azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{
			ID: azidentity.ClientID(clientID),
		})
	}

	return azidentity.NewManagedIdentityCredential(nil)
}

// azureTableHostSuffixes are the DNS suffixes of Azure Storage table endpoints across the public and
// sovereign clouds. blobEndpointFromTables only derives a blob endpoint for hosts under these, so a
// misconfigured (or attacker-controlled) AZURE_TABLES_ENDPOINT cannot make the loop send managed-identity
// bearer tokens to an arbitrary host.
var azureTableHostSuffixes = []string{
	".table.core.windows.net",       // public
	".table.core.usgovcloudapi.net", // US Gov
	".table.core.chinacloudapi.cn",  // China (21Vianet)
}

// blobEndpointFromTables derives the blob service endpoint from an Azure Tables endpoint by swapping the
// service segment (e.g. https://acct.table.core.windows.net/ -> https://acct.blob.core.windows.net/).
// It only accepts an https URL whose host is a known Azure Storage table domain; everything else (non-
// cloud/Azurite path-style, non-https, or an unexpected host) returns "" — pprof is a deployed-env-only
// diagnostic and must never point managed-identity auth at an untrusted endpoint.
func blobEndpointFromTables(tables string) string {
	tables = strings.TrimSpace(tables)
	if tables == "" {
		return ""
	}

	u, err := url.Parse(tables)
	if err != nil || u.Scheme != "https" {
		return ""
	}

	// A storage service endpoint is scheme://host only. Reject anything carrying userinfo, a non-root
	// path, a query, or a fragment — a well-formed AZURE_TABLES_ENDPOINT has none, and deriving a blob
	// endpoint from such a URL would produce a wrong (or credential-bearing) target.
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}

	host := strings.ToLower(u.Host)           // includes port, if any; DNS is case-insensitive so lowercasing is safe
	hostname := strings.ToLower(u.Hostname()) // no port, for suffix matching

	trusted := false

	for _, suffix := range azureTableHostSuffixes {
		if strings.HasSuffix(hostname, suffix) {
			trusted = true

			break
		}
	}

	if !trusted {
		return ""
	}

	// Swap the service label in the host only (not userinfo/path/query), on the parsed URL, so an
	// occurrence of ".table." elsewhere in the raw string can't corrupt the derived endpoint.
	u.Host = strings.Replace(host, ".table.", ".blob.", 1)

	return u.String()
}

func envOrDefaultStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}

	return def
}

func envSeconds(key string, def time.Duration) time.Duration {
	// Cap so n*time.Second can't overflow int64 (which would wrap to a negative/tiny duration and make
	// capture cycles fire immediately); an out-of-range value falls back to the default.
	const maxSeconds = int64(math.MaxInt64) / int64(time.Second)

	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && int64(n) <= maxSeconds {
			return time.Duration(n) * time.Second
		}
	}

	return def
}

// instanceID returns a per-process token (hostname + pid) used to keep blob names unique across replicas
// and restarts. Falls back to the pid alone if the hostname is unavailable.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return sanitizeBlobSegment(strconv.Itoa(os.Getpid()))
	}

	return sanitizeBlobSegment(fmt.Sprintf("%s-%d", host, os.Getpid()))
}

// sanitizeBlobSegment keeps blob path segments to safe characters.
func sanitizeBlobSegment(s string) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if mapped == "" {
		return "unknown"
	}

	return mapped
}
