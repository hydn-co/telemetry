// Package profiling provides a periodic runtime/pprof capture-to-blob loop for services that have no
// reachable /debug/pprof endpoint (no ingress, container exec blocked) but do write state to an Azure
// storage account. It captures CPU, heap, and goroutine profiles and uploads them to that account's blob
// service, authenticated with the service's managed identity.
//
// It is a separate subpackage on purpose: only importers of this package pull in the Azure
// blob/identity SDKs, so services that only need OTLP telemetry keep a lean dependency graph.
//
// Feature configuration is passed in via [Options] — this package reads no feature flags from the
// environment, so the caller owns any env-var naming. It does read the standard Azure environment for
// infrastructure/credentials: AZURE_TABLES_ENDPOINT (the blob endpoint is derived from it by swapping
// ".table." -> ".blob.") and AZURE_CLIENT_ID (the user-assigned managed identity to authenticate with;
// falls back to system-assigned when unset).
//
// The managed identity needs Storage Blob Data Contributor on the account. Every error is logged and the
// loop continues — profiling must never crash the caller.
package profiling

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
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
	envTables   = "AZURE_TABLES_ENDPOINT"
	envClientID = "AZURE_CLIENT_ID"

	defaultCapture   = 30 * time.Second
	defaultInterval  = 5 * time.Minute
	defaultContainer = "pprof"

	// blobOpTimeout bounds each best-effort blob operation (container ensure, profile upload) so a
	// stalled network/MSI path can't hang the capture goroutine or delay shutdown.
	blobOpTimeout = 60 * time.Second
)

// Options configures the capture loop. Enabled, Service, and Version are the meaningful inputs; the
// remaining fields fall back to defaults when zero. The caller supplies these (e.g. from its own
// environment variables) — this package defines no env-var names of its own.
type Options struct {
	// Enabled turns the loop on. When false, Start is a no-op.
	Enabled bool
	// Service and Version namespace uploaded blobs: <container>/<service>/<version>/...
	Service string
	Version string
	// Capture is the CPU profile window per cycle (default 30s when <= 0).
	Capture time.Duration
	// Interval is the sleep between cycles (default 5m when <= 0).
	Interval time.Duration
	// Container is the blob container for uploads (default "pprof" when empty).
	Container string
}

// Start launches the capture loop when opts.Enabled is set, uploading CPU, heap, and goroutine profiles to blob storage
// under <service>/<version>/. It returns a stop func (a no-op when disabled or misconfigured, so callers
// can always defer it).
//
// Call Start at most once per process: CPU profiling is process-global (runtime/pprof allows only one
// active CPU profile), so a second concurrent loop would fail every StartCPUProfile and still upload
// redundant heap profiles.
func Start(ctx context.Context, opts Options) func() {
	noop := func() {}

	if !opts.Enabled {
		return noop
	}

	// Require Service + Version: they namespace the blob prefix (<service>/<version>/), so empty values
	// would upload under "unknown/unknown" and silently mix profiles from different services.
	service := strings.TrimSpace(opts.Service)
	version := strings.TrimSpace(opts.Version)

	if service == "" || version == "" {
		slog.WarnContext(ctx, "pprof: Service and Version are required; pprof not started",
			slog.String("service", service), slog.String("version", version))

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

	// Azure blob container names must be lowercase; normalize, then fall back to the default for an empty
	// or otherwise invalid value so a misconfiguration can't fail CreateContainer/UploadBuffer every cycle.
	container := strings.ToLower(strings.TrimSpace(opts.Container))
	if container == "" {
		container = defaultContainer
	} else if !validContainerName(container) {
		slog.WarnContext(ctx, "pprof: invalid blob container name; using default",
			slog.String("container", container), slog.String("default", defaultContainer))

		container = defaultContainer
	}

	capture := opts.Capture
	if capture <= 0 {
		capture = defaultCapture
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

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

// captureCycle captures one CPU profile (over captureDur) plus heap and goroutine snapshots, uploading all three.
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

	// Goroutine dump at debug=2: full per-goroutine stacks including blocked-on state. Cheap (a
	// snapshot), and the one artifact that reveals a hang/deadlock — a stalled service shows little on
	// the CPU profile precisely because it's blocked, not running.
	var goroutines bytes.Buffer
	if err := runtimepprof.Lookup("goroutine").WriteTo(&goroutines, 2); err != nil {
		slog.ErrorContext(ctx, "pprof: goroutine profile failed", slog.String("error", err.Error()))

		return
	}

	upload(ctx, client, container, fmt.Sprintf("%s/goroutine-%s-%s.txt", prefix, ts, instance), goroutines.Bytes())
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
		if errors.Is(err, context.Canceled) {
			// Expected when Stop() cancels the loop mid-upload during shutdown; not a real failure.
			return
		}

		slog.ErrorContext(ctx, "pprof: upload failed",
			slog.String("blob", blobName), slog.String("error", err.Error()))

		return
	}

	// Debug, not Info: this fires twice per cycle per replica; Info is reserved for lifecycle events.
	slog.DebugContext(ctx, "pprof: uploaded profile", slog.String("blob", blobName), slog.Int("bytes", len(data)))
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
	// path, a query, or a fragment — a well-formed tables endpoint has none, and deriving a blob endpoint
	// from such a URL would produce a wrong (or credential-bearing) target.
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

// instanceID returns a per-process token (hostname + pid) used to keep blob names unique across replicas
// and restarts. Falls back to the pid alone if the hostname is unavailable.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return sanitizeBlobSegment(strconv.Itoa(os.Getpid()))
	}

	return sanitizeBlobSegment(fmt.Sprintf("%s-%d", host, os.Getpid()))
}

// containerNameRE matches the body of a valid Azure blob container name: lowercase letters/digits, with
// single (non-consecutive) hyphens allowed only between them (so no leading/trailing/double hyphen).
var containerNameRE = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)

// validContainerName reports whether s satisfies Azure's blob container naming rules (3–63 chars,
// lowercase alphanumerics and single interior hyphens). Callers pass an already-lowercased value.
func validContainerName(s string) bool {
	return len(s) >= 3 && len(s) <= 63 && containerNameRE.MatchString(s)
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
