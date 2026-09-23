// Command gateway runs the LLM gateway as a standalone compiled binary,
// sitting BEHIND Traefik rather than inside it.
//
// This is deployment form (B). Form (A) — the Yaegi-interpreted Traefik
// middleware plugin — is unchanged and remains the default; both forms are
// built from the same root package, which is why nothing in this directory
// may ever be imported by it.
//
// Why a binary exists at all: Yaegi hands the interpreted plugin a
// synthetic http.ResponseWriter that does not carry Traefik's own
// http.Flusher through the reflect boundary, so every per-chunk flush the
// plugin performs is a no-op and an SSE stream arrives in one burst once
// the handler returns (README's "Known limitations", and the upstream
// traefik/traefik#10269 it links). Compiled, the identical code's
// assertion in statusTrackingWriter.Flush (errors.go) succeeds against a
// real *http.response, so chunks reach the client as they are produced.
//
// THE yaml IMPORT BELOW MUST STAY IN THIS PACKAGE. The root package is
// interpreted by Yaegi in form (A) and by the Plugin Catalog's analyzer
// (piceus) at release time; keeping its non-stdlib surface to
// oss-telemetry alone is what makes both work. Nothing enforces this
// automatically — `make yaegi-check` resolves go.yaml.in/yaml/v3 out of
// vendor/ perfectly well and stays green even with the import at the root
// — so the rule lives here, in a comment, and in review.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	yaml "go.yaml.in/yaml/v3"

	traefikllmgateway "github.com/lukaszraczylo/traefik-llmgateway"
)

// version is this BINARY's version, stamped at build time with
// `-ldflags "-X main.version=1.2.3"`.
//
// It is deliberately a var, not a const: the root package's own
// pluginVersion is a const (version.go), which `-X` silently refuses to
// write — verified, the build succeeds and the value never lands in the
// binary. The root package gets its version the only way a const can, by
// workflow-prepare.sh rewriting the source literal before the build, so
// the two can disagree unless the same stamping runs for both. -version
// reports this string; the admin API and telemetry report pluginVersion.
var version = "dev"

// Default flag values. Each is overridable by the matching LLMGW_* env
// var, which in turn is overridden by an explicitly-passed flag — the
// house rule that an explicit override always wins, implemented by
// feeding the env value in as the flag's DEFAULT rather than reading it
// after parsing.
const (
	defaultConfigPath     = "/etc/llmgateway/config.yaml"
	defaultListenAddr     = ":8080"
	defaultInstanceName   = "gateway"
	defaultLogLevel       = "info"
	defaultShutdownWait   = 30 * time.Second
	defaultDrainDelay     = 5 * time.Second
	defaultHealthPath     = "/healthz"
	defaultReadyPath      = "/readyz"
	defaultReadHeaderWait = 10 * time.Second
	defaultIdleTimeout    = 120 * time.Second
	defaultMaxHeaderBytes = 1 << 20
)

// telemetryOptOutVars are the environment variables oss-telemetry itself
// reads to suppress the anonymous startup ping (its telemetry.go). The
// binary never reimplements that decision — shouldSendTelemetry is
// unexported and the real gate also depends on whether the build was
// version-stamped — it only reports at startup which opt-out an operator
// actually set, so a deployment that believes it opted out can confirm it.
var telemetryOptOutVars = []string{
	"DO_NOT_TRACK",
	"OSS_TELEMETRY_DISABLED",
	"TRAEFIK_LLMGATEWAY_DISABLE_TELEMETRY",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "llmgw-bin ERROR %v\n", err)
		os.Exit(1)
	}
}

// options carries every resolved command-line/environment setting.
type options struct {
	configPath   string
	listenAddr   string
	name         string
	logLevel     string
	healthPath   string
	readyPath    string
	shutdownWait time.Duration
	drainDelay   time.Duration
	showVersion  bool
}

// parseOptions defines and parses the binary's flags. Every flag's
// default is the matching env var's value when set, so precedence reads
// flag > env > built-in default.
func parseOptions(args []string, stderr *os.File) (*options, error) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := &options{}
	fs.StringVar(&o.configPath, "config", envOr("LLMGW_CONFIG", defaultConfigPath),
		"path to the gateway config file (YAML, or JSON when the name ends .json) [LLMGW_CONFIG]")
	fs.StringVar(&o.listenAddr, "listen", envOr("LLMGW_LISTEN", defaultListenAddr),
		"address to serve HTTP on [LLMGW_LISTEN]")
	fs.StringVar(&o.name, "name", envOr("LLMGW_NAME", defaultInstanceName),
		"instance name, used as the llmgw[<name>] log prefix [LLMGW_NAME]")
	fs.StringVar(&o.logLevel, "log-level", envOr("LLMGW_LOG_LEVEL", defaultLogLevel),
		"debug|info|warn|error. SCOPE: this filters only THIS BINARY's own lifecycle lines. "+
			"The gateway's own INFO/WARN/ERROR output is written unconditionally to stderr by the root "+
			"package (logger.go) through unexported helpers this command cannot reach, and is never filtered "+
			"[LLMGW_LOG_LEVEL]")
	fs.StringVar(&o.healthPath, "health-path", envOr("LLMGW_HEALTH_PATH", defaultHealthPath),
		"liveness path served by this binary, ahead of the gateway's own routes [LLMGW_HEALTH_PATH]")
	fs.StringVar(&o.readyPath, "ready-path", envOr("LLMGW_READY_PATH", defaultReadyPath),
		"readiness path served by this binary, ahead of the gateway's own routes [LLMGW_READY_PATH]")
	fs.DurationVar(&o.shutdownWait, "shutdown-timeout", envDurationOr("LLMGW_SHUTDOWN_TIMEOUT", defaultShutdownWait),
		"how long to wait for in-flight requests to finish before abandoning them [LLMGW_SHUTDOWN_TIMEOUT]")
	fs.DurationVar(&o.drainDelay, "drain-delay", envDurationOr("LLMGW_DRAIN_DELAY", defaultDrainDelay),
		"how long to keep serving after readiness starts failing, so the load balancer can drop this "+
			"endpoint before the listener closes [LLMGW_DRAIN_DELAY]")
	fs.BoolVar(&o.showVersion, "version", false, "print this binary's version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.healthPath == o.readyPath {
		return nil, fmt.Errorf("-health-path and -ready-path must differ (both are %q)", o.healthPath)
	}
	for _, p := range []string{o.healthPath, o.readyPath} {
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("health/ready path %q must start with /", p)
		}
	}
	return o, nil
}

// envOr returns the environment variable's value, or def when it is unset
// or empty. An empty value is treated as unset deliberately: a Kubernetes
// env entry with an empty `value:` is far more often an accident than a
// request for an empty listen address.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDurationOr is envOr for a Go duration string. An unparseable value is
// NOT silently ignored — it is reported and the default used, since
// swallowing it would leave an operator believing a timeout applies that
// never did.
func envDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmgw-bin WARN %s=%q is not a duration (%v); using %s\n", key, v, err, def)
		return def
	}
	return d
}

// metricsCIDRWarning reports the startup warning run() logs, below, when
// cfg.Metrics.AllowedCIDRs is set (F5, review-auth.md finding 5) — split
// out as its own pure function, matching how passthroughUnknown's own
// warning stays inline in run(), so the DECISION (warn or not, and with
// what message) is unit-testable without capturing this binary's real
// os.Stderr.
//
// metrics.allowedCIDRs is an OPTIONAL bypass of the /metrics admin-key
// check for a caller whose SOURCE ADDRESS is trusted (MetricsConfig's
// own doc comment, metrics.go) — meaningful only when that source
// address is the real caller's. In THIS deployment form, it never is:
// Traefik sits in FRONT of this binary (this file's own package doc
// comment), so every request this process sees arrives with RemoteAddr
// set to Traefik's own address, not the original client's. An allowlist
// entry that matches Traefik's pod/service CIDR — an easy mistake, since
// that is exactly the CIDR an operator might expect to "trust" —
// bypasses the admin key for every client that can reach Traefik, not
// just a co-located scraper.
func metricsCIDRWarning(cfg *traefikllmgateway.Config) (msg string, warn bool) {
	if cfg.Metrics == nil || len(cfg.Metrics.AllowedCIDRs) == 0 {
		return "", false
	}
	return "metrics.allowedCIDRs is set, but this binary runs BEHIND Traefik: the source address it " +
		"sees on every request is Traefik's own address, never the original caller's. An allowlist entry " +
		"that matches Traefik's own pod/service CIDR bypasses the admin key for every client that reaches " +
		"Traefik. Confirm the allowlist is scoped to this binary's actual peer address before relying on it.", true
}

// run is main's body, split out so every exit path returns an error
// instead of calling os.Exit from inside nested logic.
func run() error {
	opts, err := parseOptions(os.Args[1:], os.Stderr)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Println(version)
		return nil
	}
	log := newLogger(opts.logLevel)

	cfg, err := loadConfig(opts.configPath)
	if err != nil {
		return err
	}
	log.info("starting version=%s name=%s config=%s listen=%s", version, opts.name, opts.configPath, opts.listenAddr)
	logTelemetryOptOut(log)

	// passthroughUnknown routes an unrecognized path to the `next`
	// handler. In form (A) that is the Traefik router's own backing
	// service; in form (B) Traefik is IN FRONT of this process and there
	// is no downstream at all, so the flag cannot mean what it means in
	// the plugin. This is a warning rather than a hard error because the
	// terminal handler below keeps the behaviour well-defined (a clean
	// 404) — an operator carrying a working plugin config across should
	// not be blocked, only told.
	if cfg.PassthroughUnknown {
		log.warn("passthroughUnknown is set, but a standalone binary has no downstream to pass through to: " +
			"unknown routes return 404. Route non-gateway paths in Traefik instead.")
	}

	if msg, warn := metricsCIDRWarning(cfg); warn {
		log.warn(msg)
	}

	// signal.NotifyContext, not context.Background(): New blocks on the
	// registry's synchronous warm-fill, which with a large provider set
	// can run for minutes before the listener ever opens. A SIGTERM
	// arriving inside that window has to abort startup rather than be
	// ignored until it finishes.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	handler, err := traefikllmgateway.New(ctx, terminalHandler(), cfg, opts.name)
	if err != nil {
		return fmt.Errorf("construct gateway: %w", err)
	}
	if ctx.Err() != nil {
		log.info("signal received during startup; exiting before serving")
		closeGateway(handler, log)
		return nil
	}

	var ready atomic.Bool
	srv := &http.Server{
		Addr:    opts.listenAddr,
		Handler: withHealth(handler, &ready, opts.healthPath, opts.readyPath),
		// Bounds a Slowloris header trickle (and satisfies gosec G112).
		ReadHeaderTimeout: defaultReadHeaderWait,
		// ReadTimeout and WriteTimeout are BOTH deliberately zero.
		//
		// net/http sets each as an ABSOLUTE deadline on the connection
		// before the handler runs — neither is an idle timeout. A
		// non-zero WriteTimeout therefore expires mid-generation on any
		// stream that outlives it: writes start failing with
		// os.ErrDeadlineExceeded and the client gets a truncated SSE
		// stream with no terminal `data: [DONE]`, which is a worse
		// defect than the burst-delivery bug this binary exists to fix.
		// ReadTimeout has the same shape on the way in and would
		// truncate a large multipart audio upload over a slow link.
		//
		// For the OUTBOUND leg this is covered one layer in:
		// Config.RequestTimeout (timeout.go) is a PROGRESS-based
		// watchdog, five minutes by default, with a per-provider
		// override. Body size is capped inside the root package
		// independently.
		//
		// CORRECTION (security audit run-1): that delegation does NOT
		// cover the INBOUND leg, and this comment previously implied it
		// did. newWatchdogBody wraps only an UPSTREAM RESPONSE body — its
		// two production call sites both pass resp.Body (providers.go,
		// routes_passthrough.go) — and is never applied to r.Body. So
		// between a complete header block and the handler returning,
		// nothing here bounds a client that trickles its request body,
		// while that read holds one of the process-wide body-admission
		// slots (acquireBodyAdmission, routes_unified.go). A deployment
		// that fronts this binary with a proxy inherits that proxy's read
		// timeout; one that exposes it directly has none. Fixing it
		// properly needs a progress-based bound on r.Body for the
		// duration of the admission-held read, in the root package —
		// a finite ReadTimeout here is the wrong shape, for exactly the
		// truncation reason stated above.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    defaultIdleTimeout,
		MaxHeaderBytes: defaultMaxHeaderBytes,
	}

	ready.Store(true)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.info("listening on %s (health %s, ready %s)", opts.listenAddr, opts.healthPath, opts.readyPath)

	select {
	case err := <-errCh:
		// ErrServerClosed only ever arrives after a Shutdown this
		// function did not start, so reaching it here is a real failure.
		if !errors.Is(err, http.ErrServerClosed) {
			closeGateway(handler, log)
			return fmt.Errorf("listen on %s: %w", opts.listenAddr, err)
		}
		return nil
	case <-ctx.Done():
	}

	// Ordering below is load-bearing and must not be reordered.
	//
	// stop() first restores the default signal disposition, so an
	// impatient operator's SECOND SIGTERM kills the process immediately
	// instead of being swallowed by the handler that is already draining.
	stop()
	log.info("signal received; draining")

	// Fail readiness BEFORE the listener stops accepting. Without the
	// delay in between, the listener closes while the load balancer still
	// routes to this pod, which turns every rollout into a burst of 503s.
	ready.Store(false)
	time.Sleep(opts.drainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.shutdownWait)
	defer cancel()
	// Shutdown WAITS for in-flight handlers rather than interrupting
	// them, so shutdownWait is really "how long before we abandon a
	// long generation". Raise it together with the pod's
	// terminationGracePeriodSeconds where long streams matter.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.error("shutdown: %v", err)
	}

	// STRICTLY after Shutdown returns. Close releases the pooled Redis
	// connections shared by the limiter's store and the response cache;
	// closing them while requests are still draining would pull Redis out
	// from under in-flight handlers, and under the common redis.failOpen
	// default those requests would silently fall back to in-process
	// counters rather than fail — a correctness regression invisible in
	// the logs.
	closeGateway(handler, log)
	log.info("stopped")
	return nil
}

// closeGateway releases the Gateway's pooled resources when the handler
// New returned exposes Close.
//
// The assertion is comma-ok against a locally declared interface rather
// than a bare *traefikllmgateway.Gateway assertion: New's declared return
// type is http.Handler, so a bare assertion would panic the moment that
// return is ever wrapped, and matching on the method actually needed
// keeps this command uncoupled from the concrete type.
func closeGateway(h http.Handler, log *logger) {
	c, ok := h.(interface{ Close() error })
	if !ok {
		return
	}
	if err := c.Close(); err != nil {
		log.error("gateway close: %v", err)
	}
}

// terminalHandler is the `next` handler passed to New.
//
// It is never nil: g.next is dereferenced unconditionally once
// passthroughUnknown is true (llmgateway.go), and a nil next there would
// be recovered by recoverPanic into a permanent, confusing 500 on every
// unknown path rather than an outright crash. It is never a reverse proxy
// either — in form (B) Traefik sits in front of this process, so there is
// no downstream by construction.
//
// The envelope is byte-shaped to match the root package's own unknown-route
// reply (writeOAIError, errors.go), which is unexported and so cannot be
// called from here: OpenAI-compatible SDKs parse this JSON shape, and
// http.NotFoundHandler's text/plain "404 page not found" breaks them.
// main_test.go pins the shape so the hand-copy cannot drift silently.
func terminalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "unknown route",
				"type":    "invalid_request_error",
				"code":    "404",
			},
		})
	})
}

// withHealth serves the liveness and readiness paths ahead of the
// gateway, and delegates everything else VERBATIM.
//
// Deliberately hand-written rather than an http.ServeMux: ServeMux
// path-cleans and 301-redirects (collapsing "//" and resolving "..")
// before it dispatches, while the gateway routes on r.URL.EscapedPath()
// and routes_passthrough.go's traversal handling is built on that raw
// escaped form. Interposing a ServeMux would normalize paths before the
// gateway ever saw them, changing passthrough semantics and defeating
// those checks.
//
// Liveness is unconditional: making it depend on a provider or on Redis
// would turn any upstream outage into a restart loop. Readiness tracks
// only "constructed, and not yet shutting down" — warm-fill failures are
// non-fatal by design and Redis loss degrades to in-process counters
// under failOpen, so gating readiness on either would evict the pod for a
// condition the gateway is built to survive.
func withHealth(next http.Handler, ready *atomic.Bool, healthPath, readyPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			switch r.URL.Path {
			case healthPath:
				writePlain(w, http.StatusOK, "ok")
				return
			case readyPath:
				if ready.Load() {
					writePlain(w, http.StatusOK, "ready")
					return
				}
				writePlain(w, http.StatusServiceUnavailable, "draining")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// writePlain writes a health/readiness body. These endpoints are
// unauthenticated, so they carry a literal word and nothing else — no
// version, no provider list, no configuration.
func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}

// loadConfig reads path and decodes it into a *Config.
//
// THE ROUND-TRIP IS MANDATORY — see yamlToJSON. A .json file skips the
// YAML stage, which is safe because the JSON form is already the target
// shape (it is what `kubectl get middleware llmgateway -o json` emits)
// and decodes to an identical Config.
//
// Decoding starts from CreateConfig() for form parity with what Traefik
// itself does, so the two deployment forms cannot drift. It does NOT
// depend on CreateConfig pre-seeding anything: it returns a bare
// &Config{}, and every default in this codebase is applied at
// construction from a zero value rather than by pre-population.
//
// The decoder runs with DisallowUnknownFields (F12, review-auth.md
// finding 12): a plain json.Unmarshal silently drops a key that matches
// no Config field at any level, which is exactly the failure class this
// file already documents one incident of below (yamlToJSON's own doc
// comment) and cfg.PassthroughUnknown's own history repeats — a typo
// like "allowedCidrs" for "allowedCIDRs" would otherwise boot with that
// setting silently disabled rather than refusing to start. This is the
// STANDALONE BINARY's own decode only; the Traefik-plugin form (form A)
// stays on Traefik's own lenient decoder, which this package cannot
// change and does not attempt to here.
func loadConfig(path string) (*traefikllmgateway.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	doc := raw
	if !strings.EqualFold(filepath.Ext(path), ".json") {
		if doc, err = yamlToJSON(raw); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	cfg := traefikllmgateway.CreateConfig()
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// yamlToJSON converts a YAML document to JSON by way of a generic map.
//
// THIS INDIRECTION IS THE WHOLE POINT, AND IS NOT A STYLE CHOICE. The
// Config structs carry json tags only — there is not one yaml tag in the
// repository — so yaml.v3 falls back to lowercasing the entire Go field
// name when it decodes straight into a *Config: APIKey becomes "apikey",
// BaseURL "baseurl", MCPServers "mcpservers". Every all-lowercase key in a
// production config then matches by accident and every camelCase key
// misses, WITH A NIL ERROR.
//
// Measured against the live cluster's own config: the direct path returns
// no error and still produces a Config that boots, with all 18 providers
// present but every apiKey and baseUrl empty, mcpServers/modelAliases/
// modelMeta dropped wholesale, and metrics.allowedCIDRs emptied. Empty
// BaseURLs then fall back to defaultBaseURLByType, so every openai-type
// provider silently retargets api.openai.com and the mounted file:
// secrets are never read. 84 leaves differ from the correct decode.
// main_test.go asserts both halves of that so no future "simplification"
// can reintroduce it quietly.
func yamlToJSON(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	j, err := json.Marshal(doc)
	if err != nil {
		// The realistic trigger is a nested mapping key that YAML parses
		// as a non-string — a model id like `4.1:` or `3.5:` written
		// unquoted under modelMeta or pricing. The underlying error names
		// an offset or an unsupported map type rather than the block, so
		// it is wrapped with the fix rather than passed through bare.
		return nil, fmt.Errorf("convert yaml to json: %w "+
			"(a mapping key that YAML reads as a number or boolean cannot become a JSON object key — "+
			"quote it, e.g. \"4.1\": under modelMeta or pricing)", err)
	}
	return j, nil
}

// logTelemetryOptOut reports which anonymous-telemetry opt-out variable is
// set, if any, so a deployment that believes it opted out can confirm it
// from the startup log. It deliberately does not claim whether a ping
// will actually be sent: that also depends on the build being
// version-stamped, and the root package's decision function is
// unexported.
func logTelemetryOptOut(log *logger) {
	for _, k := range telemetryOptOutVars {
		if v := os.Getenv(k); v != "" {
			log.info("anonymous usage reporting opted out via %s=%s", k, v)
			return
		}
	}
	log.info("anonymous usage reporting not opted out (set DO_NOT_TRACK=1 to disable); " +
		"only version-stamped release builds ever send a ping")
}

// logger is this binary's own leveled logger. It governs ONLY the
// lifecycle lines written here — the gateway's own output goes straight
// to stderr from the root package's unexported helpers and cannot be
// filtered from outside it without reassigning os.Stderr, which would
// reorder interleaved writes and is not worth a goroutine and a pipe.
// The prefix is llmgw-bin, distinct from the gateway's own llmgw[name],
// so the two are never confused in one stream.
type logger struct {
	level int
}

// Log levels, ordered so a message is written when its own level is at or
// above the configured threshold.
const (
	levelDebug = iota
	levelInfo
	levelWarn
	levelError
)

// newLogger resolves a level name. An unrecognized name falls back to
// info and says so, rather than silently picking a level the operator did
// not ask for.
func newLogger(name string) *logger {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return &logger{level: levelDebug}
	case "info":
		return &logger{level: levelInfo}
	case "warn", "warning":
		return &logger{level: levelWarn}
	case "error":
		return &logger{level: levelError}
	default:
		fmt.Fprintf(os.Stderr, "llmgw-bin WARN unknown -log-level %q; using info\n", name)
		return &logger{level: levelInfo}
	}
}

func (l *logger) write(at int, label, format string, args ...any) {
	if at < l.level {
		return
	}
	fmt.Fprintf(os.Stderr, "llmgw-bin %s %s\n", label, fmt.Sprintf(format, args...))
}

func (l *logger) info(format string, args ...any)  { l.write(levelInfo, "INFO", format, args...) }
func (l *logger) warn(format string, args ...any)  { l.write(levelWarn, "WARN", format, args...) }
func (l *logger) error(format string, args ...any) { l.write(levelError, "ERROR", format, args...) }
