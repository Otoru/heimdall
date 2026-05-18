package server

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/otoru/heimdall/internal/metrics"
	"github.com/otoru/heimdall/internal/storage"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/zap"
)

const (
	headerContentType   = "Content-Type"
	headerContentLength = "Content-Length"
	headerLastModified  = "Last-Modified"
	errMethodNotAllowed = "method not allowed"
	errListProxies      = "list proxies"
	packagesPrefix      = "packages/"
)

type Storage interface {
	Get(ctx context.Context, key string) (*s3.GetObjectOutput, error)
	Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error)
	Put(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error
	List(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error)
	Delete(ctx context.Context, key string) error
	GenerateChecksums(ctx context.Context, prefix string) error
	CleanupBadChecksums(ctx context.Context, prefix string) error
}

type Server struct {
	store          Storage
	proxy          *ProxyManager
	logger         *zap.Logger
	metrics        *metrics.Registry
	user           string
	pass           string
	apiKeyEndpoint string
	apiKeyToken    string
	httpClient     *http.Client
}

func New(store Storage, logger *zap.Logger, m *metrics.Registry, user, pass, apiKeyEndpoint, apiKeyToken string) *Server {
	return &Server{
		store:          store,
		proxy:          NewProxyManager(store, logger),
		logger:         logger,
		metrics:        m,
		user:           user,
		pass:           pass,
		apiKeyEndpoint: apiKeyEndpoint,
		apiKeyToken:    apiKeyToken,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/swagger/", httpSwagger.WrapHandler)
	mux.HandleFunc("/catalog", s.authMiddleware(s.handleCatalog))
	mux.HandleFunc("/proxies", s.authMiddleware(s.routeProxies))
	mux.HandleFunc("/proxies/", s.authMiddleware(s.routeProxyByName))
	mux.HandleFunc("/packages/", s.authMiddleware(s.handlePackages))
	mux.HandleFunc("/", s.authMiddleware(s.handleObject))

	var handler http.Handler = mux
	if s.metrics != nil {
		handler = promhttp.InstrumentHandlerInFlight(
			s.metrics.InFlight,
			promhttp.InstrumentHandlerDuration(
				s.metrics.RequestDuration,
				promhttp.InstrumentHandlerCounter(
					s.metrics.RequestCount,
					handler,
				),
			),
		)
	}

	return loggingMiddleware(s.logger, handler)
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	if s.user == "" && s.pass == "" && s.apiKeyEndpoint == "" {
		return next
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
			if s.apiKeyEndpoint != "" && s.validateAPIKey(r.Context(), apiKey) {
				next(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if s.user != "" || s.pass != "" {
			u, p, ok := r.BasicAuth()
			if ok && u == s.user && p == s.pass {
				next(w, r)
				return
			}
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="heimdall"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (s *Server) validateAPIKey(ctx context.Context, key string) bool {
	base, err := url.Parse(s.apiKeyEndpoint)
	if err != nil {
		return false
	}
	base.Path = path.Join(base.Path, "licenses", "valid")
	q := url.Values{}
	q.Set("serial_number", key)
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return false
	}
	if s.apiKeyToken != "" {
		req.Header.Set("Authorization", s.apiKeyToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// @Summary Health check
// @Tags health
// @Produce plain
// @Success 200 {string} string "ok"
// @Router /healthz [get]
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// @Summary List artifacts
// @Tags catalog
// @Param path query string false "Path prefix (non-recursive); root by default"
// @Param limit query int false "Max items" default(100)
// @Produce json
// @Success 200 {array} storage.Entry
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /catalog [get]
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("path")
	limit := parseCatalogLimit(r.URL.Query().Get("limit"))

	if strings.HasPrefix(strings.TrimPrefix(prefix, "/"), "packages") {
		keys, err := s.listPackages(r.Context(), prefix, limit)
		if err != nil {
			s.writeError(w, "list packages", err)
			return
		}
		s.writeJSON(w, keys)
		return
	}

	keys, err := s.store.List(r.Context(), prefix, limit)
	if err != nil {
		s.writeError(w, "list objects", err)
		return
	}

	keys = s.mergeProxyCatalog(r.Context(), prefix, limit, keys)
	keys = filterProxyConfig(keys)

	if prefix == "" || prefix == "/" {
		keys = s.appendRootEntries(r.Context(), keys)
	}

	s.writeJSON(w, keys)
}

func parseCatalogLimit(v string) int32 {
	if v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 1000 {
			return int32(parsed)
		}
	}
	return 100
}

func filterProxyConfig(entries []storage.Entry) []storage.Entry {
	var out []storage.Entry
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, proxyConfigPrefix) {
			out = append(out, e)
		}
	}
	if out == nil {
		return []storage.Entry{}
	}
	return out
}

func (s *Server) mergeProxyCatalog(ctx context.Context, prefix string, limit int32, local []storage.Entry) []storage.Entry {
	prEntries, handled, err := s.maybeListProxy(ctx, prefix, limit)
	if err != nil {
		s.logger.Warn("list proxy path", zap.Error(err))
		return local
	}
	if !handled {
		return local
	}
	merged := append([]storage.Entry{}, prEntries...)
	seen := make(map[string]struct{}, len(merged))
	for _, e := range merged {
		seen[e.Name] = struct{}{}
	}
	for _, e := range local {
		if strings.HasPrefix(e.Path, proxyConfigPrefix) {
			continue
		}
		if _, ok := seen[e.Name]; !ok {
			merged = append(merged, e)
		}
	}
	return merged
}

func (s *Server) appendRootEntries(ctx context.Context, keys []storage.Entry) []storage.Entry {
	keys = append(keys, storage.Entry{Name: packagesPrefix, Path: packagesPrefix, Type: "group"})
	proxies, err := s.proxy.List(ctx)
	if err != nil {
		s.logger.Warn("list proxies for catalog", zap.Error(err))
		return keys
	}
	for _, pr := range proxies {
		keys = append(keys, storage.Entry{Name: pr.Name + "/", Path: pr.Name + "/", Type: "proxy"})
	}
	return keys
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Warn("encode response", zap.Error(err))
	}
}

func (s *Server) routeProxies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListProxies(w, r)
	case http.MethodPost:
		s.handleCreateProxy(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

func (s *Server) routeProxyByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/proxies/")
	name = strings.Trim(name, "/")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.handleUpdateProxy(w, r, name)
	case http.MethodDelete:
		s.handleDeleteProxy(w, r, name)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

// @Summary List proxy repositories
// @Tags proxies
// @Produce json
// @Success 200 {array} server.Proxy
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /proxies [get]
func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	proxies, err := s.proxy.List(r.Context())
	if err != nil {
		s.writeError(w, errListProxies, err)
		return
	}
	s.writeJSON(w, proxies)
}

// @Summary Create proxy repository
// @Tags proxies
// @Accept json
// @Produce json
// @Param proxy body Proxy true "Proxy configuration"
// @Success 201 {string} string "Created"
// @Failure 400 {string} string
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /proxies [post]
func (s *Server) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	var pr Proxy
	if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := s.proxy.Add(r.Context(), pr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// @Summary Update proxy repository
// @Tags proxies
// @Accept json
// @Produce json
// @Param name path string true "Proxy name"
// @Param proxy body Proxy true "Proxy configuration"
// @Success 200 {string} string "Updated"
// @Failure 400 {string} string
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /proxies/{name} [put]
func (s *Server) handleUpdateProxy(w http.ResponseWriter, r *http.Request, name string) {
	var pr Proxy
	if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := s.proxy.Update(r.Context(), name, pr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// @Summary Delete proxy repository
// @Tags proxies
// @Produce plain
// @Param name path string true "Proxy name"
// @Success 204 {string} string "Deleted"
// @Failure 400 {string} string
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /proxies/{name} [delete]
func (s *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.proxy.Delete(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// @Summary Group repository (packages) GET/HEAD
// @Tags packages
// @Produce application/octet-stream
// @Failure 404 {string} string "Not Found"
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /packages/{artifactPath} [get]
// @Router /packages/{artifactPath} [head]
func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/packages/")
	if key == "" || key == "packages" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handlePackageGet(w, r, key)
	case http.MethodHead:
		s.handlePackageHead(w, r, key)
	default:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

func (s *Server) maybeListProxy(ctx context.Context, prefix string, limit int32) ([]storage.Entry, bool, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if clean == "" {
		return nil, false, nil
	}

	entries, handled, err := s.proxy.ListPath(ctx, clean, limit)
	if err != nil || !handled {
		return entries, handled, err
	}

	for i := range entries {
		entries[i].Path = path.Join(clean, entries[i].Name)
	}
	return entries, true, nil
}

type packageAccumulator struct {
	seen      map[string]struct{}
	keys      []storage.Entry
	remaining int32
}

func newPackageAccumulator(limit int32) *packageAccumulator {
	if limit <= 0 {
		limit = 100
	}
	return &packageAccumulator{seen: make(map[string]struct{}), remaining: limit}
}

func (a *packageAccumulator) add(e storage.Entry) bool {
	e, ok := normalizePackageEntry(e)
	_, dup := a.seen[e.Name]
	if !ok || dup {
		return false
	}
	a.seen[e.Name] = struct{}{}
	a.keys = append(a.keys, e)
	a.remaining--
	return a.remaining == 0
}

func normalizePackageEntry(e storage.Entry) (storage.Entry, bool) {
	trimmed := strings.TrimPrefix(e.Path, packagesPrefix)
	if strings.HasPrefix(trimmed, proxyConfigPrefix) || strings.HasPrefix(e.Name, proxyConfigPrefix) {
		return e, false
	}
	if e.Type == "dir" || e.Type == "proxy" || e.Type == "group" {
		if !strings.HasSuffix(e.Name, "/") {
			e.Name += "/"
		}
		if !strings.HasSuffix(e.Path, "/") {
			e.Path += "/"
		}
	}
	return e, true
}

func (s *Server) logProxyListError(proxyName string, err error) {
	var se ProxyStatusError
	if errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden) {
		return
	}
	s.logger.Warn("list packages proxy", zap.String("proxy", proxyName), zap.Error(err))
}

func trimPackagePrefix(prefix string) string {
	clean := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	clean = strings.TrimPrefix(clean, "packages")
	return strings.TrimPrefix(clean, "/")
}

func (s *Server) listPackages(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error) {
	clean := trimPackagePrefix(prefix)
	acc := newPackageAccumulator(limit)

	local, err := s.store.List(ctx, clean, acc.remaining)
	if err != nil {
		s.logger.Warn("list packages local", zap.Error(err))
	}
	for _, e := range local {
		e.Path = path.Join("packages", e.Path)
		if acc.add(e) {
			return acc.keys, nil
		}
	}

	proxies, err := s.proxy.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, pr := range proxies {
		prEntries, _, err := s.proxy.ListPath(ctx, path.Join(pr.Name, clean), acc.remaining)
		if err != nil {
			s.logProxyListError(pr.Name, err)
			continue
		}
		for _, e := range prEntries {
			e.Path = path.Join("packages", pr.Name, e.Name)
			if acc.add(e) {
				return acc.keys, nil
			}
		}
	}

	return acc.keys, nil
}

func (s *Server) handlePackageGet(w http.ResponseWriter, r *http.Request, key string) {
	var resp *s3.GetObjectOutput
	// local direct
	if resp, ok := s.tryLocalGet(r.Context(), key); ok {
		defer resp.Body.Close()
		s.writeObjectResponse(w, resp)
		return
	}

	// check cached proxies
	proxies, err := s.proxy.List(r.Context())
	if err != nil {
		s.writeError(w, errListProxies, err)
		return
	}
	for _, pr := range proxies {
		resp, err := s.store.Get(r.Context(), path.Join(pr.Name, key))
		if err == nil {
			defer resp.Body.Close()
			s.writeObjectResponse(w, resp)
			return
		}
		if !storage.IsNotFound(err) {
			s.writeError(w, "fetch cached proxy object", err)
			return
		}
	}

	// fetch from upstream
	cacheKey, found, err := s.proxy.FetchFromAny(r.Context(), key)
	if err != nil {
		s.writeError(w, "proxy fetch", err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	resp, err = s.store.Get(r.Context(), cacheKey)
	if err != nil {
		if storage.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.writeError(w, "fetch cached proxy object", err)
		return
	}
	defer resp.Body.Close()
	s.writeObjectResponse(w, resp)
}

func (s *Server) handlePackageHead(w http.ResponseWriter, r *http.Request, key string) {
	if resp, ok := s.tryLocalHead(r.Context(), key); ok {
		s.writeHeadResponse(w, resp)
		return
	}

	proxies, err := s.proxy.List(r.Context())
	if err != nil {
		s.writeError(w, errListProxies, err)
		return
	}
	for _, pr := range proxies {
		resp, err := s.store.Head(r.Context(), path.Join(pr.Name, key))
		if err == nil {
			s.writeHeadResponse(w, resp)
			return
		}
		if !storage.IsNotFound(err) {
			s.writeError(w, "head cached proxy object", err)
			return
		}
	}

	presp, found, err := s.proxy.HeadFromAny(r.Context(), key)
	if err != nil {
		s.writeError(w, "proxy head", err)
		return
	}
	if found {
		defer presp.Body.Close()
		writeProxyHeaders(w, presp)
		w.WriteHeader(http.StatusOK)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) tryLocalGet(ctx context.Context, key string) (*s3.GetObjectOutput, bool) {
	resp, err := s.store.Get(ctx, key)
	if err == nil {
		return resp, true
	}
	if !storage.IsNotFound(err) {
		return nil, false
	}

	roots, err := s.store.List(ctx, "", 1000)
	if err != nil {
		return nil, false
	}
	for _, e := range roots {
		if e.Type != "dir" {
			continue
		}
		resp, err := s.store.Get(ctx, path.Join(e.Name, key))
		if err == nil {
			return resp, true
		}
	}
	return nil, false
}

func (s *Server) tryLocalHead(ctx context.Context, key string) (*s3.HeadObjectOutput, bool) {
	resp, err := s.store.Head(ctx, key)
	if err == nil {
		return resp, true
	}
	if !storage.IsNotFound(err) {
		return nil, false
	}

	roots, err := s.store.List(ctx, "", 1000)
	if err != nil {
		return nil, false
	}
	for _, e := range roots {
		if e.Type != "dir" {
			continue
		}
		resp, err := s.store.Head(ctx, path.Join(e.Name, key))
		if err == nil {
			return resp, true
		}
	}
	return nil, false
}

func writeProxyHeaders(w http.ResponseWriter, resp *http.Response) {
	if cl := resp.Header.Get(headerContentLength); cl != "" {
		w.Header().Set(headerContentLength, cl)
	}
	if ct := resp.Header.Get(headerContentType); ct != "" {
		w.Header().Set(headerContentType, ct)
	}
	if lm := resp.Header.Get(headerLastModified); lm != "" {
		w.Header().Set(headerLastModified, lm)
	}
}

func (s *Server) fetchViaProxy(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	found, err := s.proxy.FetchAndCache(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	resp, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *Server) writeHeadResponse(w http.ResponseWriter, resp *s3.HeadObjectOutput) {
	if resp.ContentLength != nil && *resp.ContentLength >= 0 {
		w.Header().Set(headerContentLength, strconv.FormatInt(*resp.ContentLength, 10))
	}
	if resp.ContentType != nil {
		w.Header().Set(headerContentType, *resp.ContentType)
	}
	if resp.ETag != nil {
		w.Header().Set("ETag", strings.Trim(*resp.ETag, "\""))
	}
	if resp.LastModified != nil {
		w.Header().Set(headerLastModified, resp.LastModified.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) writeObjectResponse(w http.ResponseWriter, resp *s3.GetObjectOutput) {
	if resp.ContentLength != nil && *resp.ContentLength >= 0 {
		w.Header().Set(headerContentLength, strconv.FormatInt(*resp.ContentLength, 10))
	}
	if resp.ContentType != nil {
		w.Header().Set(headerContentType, *resp.ContentType)
	}
	if resp.ETag != nil {
		w.Header().Set("ETag", strings.Trim(*resp.ETag, "\""))
	}
	if resp.LastModified != nil {
		w.Header().Set(headerLastModified, resp.LastModified.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logger.Warn("stream object", zap.Error(err))
	}
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	if key == "" || key == "healthz" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, key)
	case http.MethodHead:
		s.handleHead(w, r, key)
	case http.MethodPut:
		s.handlePut(w, r, key)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

// @Summary Download artifact
// @Tags artifacts
// @Param artifactPath path string true "Artifact path (maps to S3 key with optional prefix)"
// @Produce application/octet-stream
// @Success 200 {file} file
// @Failure 404 {string} string "Not Found"
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /{artifactPath} [get]
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	resp, err := s.store.Get(r.Context(), key)
	if err != nil && !storage.IsNotFound(err) {
		s.writeError(w, "fetch object", err)
		return
	}
	if storage.IsNotFound(err) {
		resp, err = s.fetchViaProxy(r.Context(), key)
		if err != nil {
			s.writeError(w, "proxy fetch", err)
			return
		}
		if resp == nil {
			http.NotFound(w, r)
			return
		}
	}
	defer resp.Body.Close()
	s.writeObjectResponse(w, resp)
}

// @Summary Artifact metadata
// @Tags artifacts
// @Param artifactPath path string true "Artifact path (maps to S3 key with optional prefix)"
// @Success 200 {string} string "OK"
// @Failure 404 {string} string "Not Found"
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /{artifactPath} [head]
func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, key string) {
	resp, err := s.store.Head(r.Context(), key)
	if err != nil && !storage.IsNotFound(err) {
		s.writeError(w, "head object", err)
		return
	}
	if storage.IsNotFound(err) {
		presp, found, perr := s.proxy.Head(r.Context(), key)
		if perr != nil {
			s.writeError(w, "proxy head", perr)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		defer presp.Body.Close()
		writeProxyHeaders(w, presp)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.writeHeadResponse(w, resp)
}

// @Summary Upload artifact
// @Tags artifacts
// @Param artifactPath path string true "Artifact path (maps to S3 key with optional prefix)"
// @Accept application/octet-stream
// @Produce plain
// @Success 201 {string} string "Created"
// @Security BasicAuth
// @Security ApiKeyAuth
// @Router /{artifactPath} [put]
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	defer r.Body.Close()

	if r.ContentLength < 0 {
		http.Error(w, "Content-Length required", http.StatusLengthRequired)
		return
	}

	contentType := r.Header.Get(headerContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	tmp, err := os.CreateTemp("", "heimdall-upload-*")
	if err != nil {
		s.writeError(w, "buffer upload", err)
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	if _, err := io.CopyN(tmp, r.Body, r.ContentLength); err != nil && !errors.Is(err, io.EOF) {
		s.writeError(w, "buffer upload copy", err)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		s.writeError(w, "buffer upload seek", err)
		return
	}

	sha1h := sha1.New()
	md5h := md5.New()
	if _, err := io.Copy(io.MultiWriter(sha1h, md5h), tmp); err != nil {
		s.writeError(w, "compute checksum", err)
		return
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		s.writeError(w, "buffer upload seek start", err)
		return
	}

	err = s.store.Put(r.Context(), key, tmp, contentType, r.ContentLength)
	if err != nil {
		s.writeError(w, "store object", err)
		return
	}

	sha1sum := hex.EncodeToString(sha1h.Sum(nil))
	md5sum := hex.EncodeToString(md5h.Sum(nil))

	if err := s.store.Put(r.Context(), key+".sha1", strings.NewReader(sha1sum), "text/plain", int64(len(sha1sum))); err != nil {
		s.writeError(w, "store sha1", err)
		return
	}
	if err := s.store.Put(r.Context(), key+".md5", strings.NewReader(md5sum), "text/plain", int64(len(md5sum))); err != nil {
		s.writeError(w, "store md5", err)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (s *Server) writeError(w http.ResponseWriter, action string, err error) {
	if storage.IsNotFound(err) {
		http.NotFound(w, nil)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		w.WriteHeader(499)
		return
	}
	var se ProxyStatusError
	if errors.As(err, &se) {
		http.Error(w, http.StatusText(se.Code), se.Code)
		return
	}
	s.logger.Error(action, zap.Error(err))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func loggingMiddleware(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		if logger != nil {
			logger.Info("request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", lrw.status),
				zap.Duration("duration", time.Since(start)),
			)
		}
	})
}
