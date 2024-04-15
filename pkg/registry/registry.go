package registry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/opencontainers/go-digest"

	"github.com/spegel-org/spegel/internal/mux"
	"github.com/spegel-org/spegel/pkg/metrics"
	"github.com/spegel-org/spegel/pkg/oci"
	"github.com/spegel-org/spegel/pkg/routing"
	"github.com/spegel-org/spegel/pkg/throttle"
)

const (
	MirroredHeaderKey = "X-Spegel-Mirrored"
)

type Registry struct {
	log              logr.Logger
	throttler        *throttle.Throttler
	ociClient        oci.Client
	router           routing.Router
	httpClient       *http.Client
	localAddr        string
	resolveRetries   int
	resolveTimeout   time.Duration
	resolveLatestTag bool
}

type Option func(*Registry)

func WithResolveRetries(resolveRetries int) Option {
	return func(r *Registry) {
		r.resolveRetries = resolveRetries
	}
}

func WithResolveLatestTag(resolveLatestTag bool) Option {
	return func(r *Registry) {
		r.resolveLatestTag = resolveLatestTag
	}
}

func WithResolveTimeout(resolveTimeout time.Duration) Option {
	return func(r *Registry) {
		r.resolveTimeout = resolveTimeout
	}
}

func WithTransport(transport http.RoundTripper) Option {
	return func(r *Registry) {
		r.httpClient.Transport = transport
	}
}

func WithLocalAddress(localAddr string) Option {
	return func(r *Registry) {
		r.localAddr = localAddr
	}
}

func WithBlobSpeed(blobSpeed throttle.Byterate) Option {
	return func(r *Registry) {
		r.throttler = throttle.NewThrottler(blobSpeed)
	}
}

func WithLogger(log logr.Logger) Option {
	return func(r *Registry) {
		r.log = log
	}
}

func NewRegistry(ociClient oci.Client, router routing.Router, opts ...Option) *Registry {
	r := &Registry{
		ociClient:        ociClient,
		router:           router,
		httpClient:       &http.Client{},
		resolveRetries:   3,
		resolveTimeout:   1 * time.Second,
		resolveLatestTag: true,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Registry) Server(addr string) *http.Server {
	srv := &http.Server{
		Addr:    addr,
		Handler: mux.NewServeMux(r.handle),
	}
	return srv
}

func (r *Registry) handle(rw mux.ResponseWriter, req *http.Request) {
	start := time.Now()
	handler := req.URL.Path
	if strings.HasPrefix(handler, "/v2") {
		handler = "/v2/*"
	}

	defer func() {
		latency := time.Since(start)
		statusCode := strconv.FormatInt(int64(rw.Status()), 10)

		metrics.HttpRequestsInflight.WithLabelValues(handler).Add(-1)
		metrics.HttpRequestDurHistogram.WithLabelValues(handler, req.Method, statusCode).Observe(latency.Seconds())
		metrics.HttpResponseSizeHistogram.WithLabelValues(handler, req.Method, statusCode).Observe(float64(rw.Size()))

		// Ignore logging requests to healthz to reduce log noise
		if req.URL.Path == "/healthz" {
			return
		}

		// Logging
		ip := getClientIP(req)
		path := req.URL.Path
		kvs := []interface{}{"path", path, "status", rw.Status(), "method", req.Method, "latency", latency, "ip", ip}
		if rw.Status() >= 200 && rw.Status() < 300 {
			r.log.Info("", kvs...)
			return
		}
		r.log.Error(rw.Error(), "", kvs...)
	}()

	metrics.HttpRequestsInflight.WithLabelValues(handler).Add(1)

	if req.URL.Path == "/healthz" && req.Method == http.MethodGet {
		r.readyHandler(rw, req)
		return
	}
	if strings.HasPrefix(req.URL.Path, "/v2") && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		r.registryHandler(rw, req)
		return
	}
	rw.WriteHeader(http.StatusNotFound)
}

func (r *Registry) readyHandler(rw mux.ResponseWriter, req *http.Request) {
	ok, err := r.router.Ready()
	if err != nil {
		rw.WriteError(http.StatusInternalServerError, err)
		return
	}
	if !ok {
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (r *Registry) registryHandler(rw mux.ResponseWriter, req *http.Request) {
	// Quickly return 200 for /v2 to indicate that registry supports v2.
	if path.Clean(req.URL.Path) == "/v2" {
		return
	}

	// Parse out path components from request.
	registryName := req.URL.Query().Get("ns")
	ref, dgst, refType, err := parsePathComponents(registryName, req.URL.Path)
	if err != nil {
		rw.WriteError(http.StatusNotFound, err)
		return
	}

	// Check if latest tag should be resolved
	if !r.resolveLatestTag && ref != "" {
		_, tag, _ := strings.Cut(ref, ":")
		if tag == "latest" {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
	}

	// Requests without mirror header set will be mirrored
	if req.Header.Get(MirroredHeaderKey) != "true" {
		key := dgst.String()
		if key == "" {
			key = ref
		}
		r.handleMirror(rw, req, key)
		sourceType := "internal"
		if r.isExternalRequest(req) {
			sourceType = "external"
		}
		cacheType := "hit"
		if rw.Status() != http.StatusOK {
			cacheType = "miss"
		}
		metrics.MirrorRequestsTotal.WithLabelValues(registryName, cacheType, sourceType).Inc()
		return
	}

	// Serve registry endpoints.
	if dgst == "" {
		dgst, err = r.ociClient.Resolve(req.Context(), ref)
		if err != nil {
			rw.WriteError(http.StatusNotFound, err)
			return
		}
	}

	switch refType {
	case referenceTypeManifest:
		r.handleManifest(rw, req, dgst)
	case referenceTypeBlob:
		r.handleBlob(rw, req, dgst)
	default:
		// If nothing matches return 404.
		rw.WriteHeader(http.StatusNotFound)
	}
}

func (r *Registry) handleMirror(rw mux.ResponseWriter, req *http.Request, key string) {
	log := r.log.WithValues("key", key, "path", req.URL.Path, "ip", req.RemoteAddr)

	// Resolve mirror with the requested key
	resolveCtx, cancel := context.WithTimeout(req.Context(), r.resolveTimeout)
	defer cancel()
	resolveCtx = logr.NewContext(resolveCtx, log)
	isExternal := r.isExternalRequest(req)
	if isExternal {
		log.Info("handling mirror request from external node")
	}
	peerCh, err := r.router.Resolve(resolveCtx, key, isExternal, r.resolveRetries)
	if err != nil {
		rw.WriteError(http.StatusInternalServerError, err)
		return
	}

	// TODO: Refactor context cancel and mirror channel closing
	for {
		select {
		case <-resolveCtx.Done():
			// Request has been closed by server or client. No use continuing.
			rw.WriteError(http.StatusNotFound, fmt.Errorf("request closed for key: %s", key))
			return
		case ipAddr, ok := <-peerCh:
			// Channel closed means no more mirrors will be received and max retries has been reached.
			if !ok {
				rw.WriteError(http.StatusNotFound, fmt.Errorf("mirror resolve retries exhausted for key: %s", key))
				return
			}

			scheme := "http"
			if req.TLS != nil {
				scheme = "https"
			}
			u := url.URL{
				Scheme: scheme,
				Host:   ipAddr.String(),
				Path:   req.URL.Path,
				// TODO: Should this error early if not set?
				RawQuery: fmt.Sprintf("ns=%s", req.URL.Query().Get("ns")),
			}
			forwardReq, err := http.NewRequestWithContext(req.Context(), req.Method, u.String(), nil)
			if err != nil {
				rw.WriteError(http.StatusInternalServerError, err)
				return
			}
			forwardReq.Header.Add(MirroredHeaderKey, "true")
			resp, err := r.httpClient.Do(forwardReq)
			if err != nil {
				log.Error(err, "mirror failed attempting next")
				break
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Error(fmt.Errorf("expected mirror to respond with 200 OK but received: %s", resp.Status), "mirror failed attempting next")
				break
			}
			for k, v := range resp.Header {
				for _, vv := range v {
					rw.Header().Add(k, vv)
				}
			}
			_, err = io.Copy(rw, resp.Body)
			if err != nil {
				rw.WriteError(http.StatusInternalServerError, err)
				return
			}
			log.V(5).Info("mirrored request", "url", u.String())
			return
		}
	}
}

func (r *Registry) handleManifest(rw mux.ResponseWriter, req *http.Request, dgst digest.Digest) {
	b, mediaType, err := r.ociClient.GetManifest(req.Context(), dgst)
	if err != nil {
		rw.WriteError(http.StatusNotFound, err)
		return
	}
	rw.Header().Set("Content-Type", mediaType)
	rw.Header().Set("Content-Length", strconv.FormatInt(int64(len(b)), 10))
	rw.Header().Set("Docker-Content-Digest", dgst.String())
	if req.Method == http.MethodHead {
		return
	}
	_, err = rw.Write(b)
	if err != nil {
		rw.WriteError(http.StatusNotFound, err)
		return
	}
}

func (r *Registry) handleBlob(rw mux.ResponseWriter, req *http.Request, dgst digest.Digest) {
	size, err := r.ociClient.Size(req.Context(), dgst)
	if err != nil {
		rw.WriteError(http.StatusInternalServerError, err)
		return
	}
	rw.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	rw.Header().Set("Docker-Content-Digest", dgst.String())
	if req.Method == http.MethodHead {
		return
	}
	var w io.Writer = rw
	if r.throttler != nil {
		w = r.throttler.Writer(rw)
	}
	rc, err := r.ociClient.GetBlob(req.Context(), dgst)
	if err != nil {
		rw.WriteError(http.StatusInternalServerError, err)
		return
	}
	defer rc.Close()
	_, err = io.Copy(w, rc)
	if err != nil {
		rw.WriteError(http.StatusInternalServerError, err)
		return
	}
}

func (r *Registry) isExternalRequest(req *http.Request) bool {
	return req.Host != r.localAddr
}

func getClientIP(req *http.Request) string {
	forwardedFor := req.Header.Get("X-Forwarded-For")
	if forwardedFor != "" {
		comps := strings.Split(forwardedFor, ",")
		if len(comps) > 1 {
			return comps[0]
		}
		return forwardedFor
	}
	h, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return ""
	}
	return h
}
