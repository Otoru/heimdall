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
	store   Storage
	proxy   *ProxyManager
	logger  *zap.Logger
	metrics *metrics.Registry
	user    string
	pass    string
}

func New(store Storage, logger *zap.Logger, m *metrics.Registry, user, pass string) *Server {
	return &Server{
		store:   store,
		proxy:   NewProxyManager(store, logger),
		logger:  logger,
		metrics: m,
		user:    user,
		pass:    pass,
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
	if s.user == "" && s.pass == "" {
		return next
	}

	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != s.user || p != s.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="heimdall"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
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
// @Router /catalog [get]
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("path")
	limit := int32(100)
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 1000 {
			limit = int32(parsed)
		}
	}

	if strings.HasPrefix(strings.TrimPrefix(prefix, "/"), "packages") {
		keys, err := s.listPackages(r.Context(), prefix, limit)
		if err != nil {
			s.writeError(w, "list packages", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(keys); err != nil {
			s.logger.Warn("encode catalog", zap.Error(err))
		}
		return
	}

	keys, err := s.store.List(r.Context(), prefix, limit)
	if err != nil {
		s.writeError(w, "list objects", err)
		return
	}

	if prEntries, handled, err := s.maybeListProxy(r.Context(), prefix, limit); err == nil && handled {
		// merge proxy entries with any cached local items for this prefix
		merged := append([]storage.Entry{}, prEntries...)
		existing := map[string]struct{}{}
		for _, e := range merged {
			existing[e.Name] = struct{}{}
		}
		for _, e := range keys {
			if strings.HasPrefix(e.Path, proxyConfigPrefix) {
				continue
			}
			if _, ok := existing[e.Name]; ok {
				continue
			}
			merged = append(merged, e)
		}
		keys = merged
	} else if err != nil {
		s.logger.Warn("list proxy path", zap.Error(err))
	}

	var filtered []storage.Entry
	for _, k := range keys {
		if strings.HasPrefix(k.Path, proxyConfigPrefix) {
			continue
		}
		filtered = append(filtered, k)
	}
	keys = filtered
	if keys == nil {
		keys = []storage.Entry{}
	}

	if prefix == "" || prefix == "/" {
		keys = append(keys, storage.Entry{
			Name: "packages/",
			Path: "packages/",
			Type: "group",
		})
		if proxies, err := s.proxy.List(r.Context()); err == nil {
			for _, pr := range proxies {
				keys = append(keys, storage.Entry{
					Name: pr.Name + "/",
					Path: pr.Name + "/",
					Type: "proxy",
				})
			}
		} else {
			s.logger.Warn("list proxies for catalog", zap.Error(err))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(keys); err != nil {
		s.logger.Warn("encode catalog", zap.Error(err))
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// @Summary List proxy repositories
// @Tags proxies
// @Produce json
// @Success 200 {array} server.Proxy
// @Security BasicAuth
// @Router /proxies [get]
func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	proxies, err := s.proxy.List(r.Context())
	if err != nil {
		s.writeError(w, "list proxies", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(proxies); err != nil {
		s.logger.Warn("encode proxies", zap.Error(err))
	}
}

// @Summary Create proxy repository
// @Tags proxies
// @Accept json
// @Produce json
// @Param proxy body Proxy true "Proxy configuration"
// @Success 201 {string} string "Created"
// @Failure 400 {string} string
// @Security BasicAuth
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

func (s *Server) listPackages(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	clean = strings.TrimPrefix(clean, "packages")
	clean = strings.TrimPrefix(clean, "/")

	var keys []storage.Entry
	remaining := limit
	if remaining <= 0 {
		remaining = 100
	}

	seen := map[string]struct{}{}
	add := func(e storage.Entry) {
		trimmed := strings.TrimPrefix(e.Path, "packages/")
		if strings.HasPrefix(trimmed, proxyConfigPrefix) || strings.HasPrefix(e.Name, proxyConfigPrefix) {
			return
		}
		if e.Type == "dir" || e.Type == "proxy" || e.Type == "group" {
			if !strings.HasSuffix(e.Name, "/") {
				e.Name += "/"
			}
			if !strings.HasSuffix(e.Path, "/") {
				e.Path += "/"
			}
		}
		if _, ok := seen[e.Name]; ok {
			return
		}
		seen[e.Name] = struct{}{}
		keys = append(keys, e)
		remaining--
	}

	// local
	local, err := s.store.List(ctx, clean, remaining)
	if err == nil {
		for _, e := range local {
			e.Path = path.Join("packages", e.Path)
			add(e)
			if remaining == 0 {
				return keys, nil
			}
		}
	} else {
		s.logger.Warn("list packages local", zap.Error(err))
	}

	// proxies
	proxies, err := s.proxy.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, pr := range proxies {
		prEntries, _, err := s.proxy.ListPath(ctx, path.Join(pr.Name, clean), remaining)
		if err != nil {
			s.logger.Warn("list packages proxy", zap.String("proxy", pr.Name), zap.Error(err))
			continue
		}
		for _, e := range prEntries {
			e.Path = path.Join("packages", pr.Name, e.Name)
			add(e)
			if remaining == 0 {
				return keys, nil
			}
		}
	}

	return keys, nil
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
		s.writeError(w, "list proxies", err)
		return
	}
	for _, pr := range proxies {
		resp, err := s.store.Get(r.Context(), path.Join(pr.Name, key))
		if err == nil {
			defer resp.Body.Close()
			s.writeObjectResponse(w, resp)
			return
		}
		if err != nil && !storage.IsNotFound(err) {
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
		s.writeError(w, "list proxies", err)
		return
	}
	for _, pr := range proxies {
		resp, err := s.store.Head(r.Context(), path.Join(pr.Name, key))
		if err == nil {
			s.writeHeadResponse(w, resp)
			return
		}
		if err != nil && !storage.IsNotFound(err) {
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
		if cl := presp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}
		if ct := presp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if lm := presp.Header.Get("Last-Modified"); lm != "" {
			w.Header().Set("Last-Modified", lm)
		}
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
	if err != nil && !storage.IsNotFound(err) {
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
	if err != nil && !storage.IsNotFound(err) {
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

func (s *Server) writeHeadResponse(w http.ResponseWriter, resp *s3.HeadObjectOutput) {
	if resp.ContentLength != nil && *resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(*resp.ContentLength, 10))
	}
	if resp.ContentType != nil {
		w.Header().Set("Content-Type", *resp.ContentType)
	}
	if resp.ETag != nil {
		w.Header().Set("ETag", strings.Trim(*resp.ETag, "\""))
	}
	if resp.LastModified != nil {
		w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) writeObjectResponse(w http.ResponseWriter, resp *s3.GetObjectOutput) {
	if resp.ContentLength != nil && *resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(*resp.ContentLength, 10))
	}
	if resp.ContentType != nil {
		w.Header().Set("Content-Type", *resp.ContentType)
	}
	if resp.ETag != nil {
		w.Header().Set("ETag", strings.Trim(*resp.ETag, "\""))
	}
	if resp.LastModified != nil {
		w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// @Summary Download artifact
// @Tags artifacts
// @Param artifactPath path string true "Artifact path (maps to S3 key with optional prefix)"
// @Produce application/octet-stream
// @Success 200 {file} file
// @Failure 404 {string} string "Not Found"
// @Security BasicAuth
// @Router /{artifactPath} [get]
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	resp, err := s.store.Get(r.Context(), key)
	if err != nil {
		if storage.IsNotFound(err) {
			if found, perr := s.proxy.FetchAndCache(r.Context(), key); perr != nil {
				s.writeError(w, "proxy fetch", perr)
				return
			} else if found {
				resp, err = s.store.Get(r.Context(), key)
				if err != nil {
					s.writeError(w, "fetch cached proxy object", err)
					return
				}
				defer resp.Body.Close()
			} else {
				http.NotFound(w, r)
				return
			}
		} else {
			s.writeError(w, "fetch object", err)
			return
		}
	}
	defer resp.Body.Close()

	if resp.ContentLength != nil && *resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(*resp.ContentLength, 10))
	}
	if resp.ContentType != nil {
		w.Header().Set("Content-Type", *resp.ContentType)
	}
	if resp.ETag != nil {
		w.Header().Set("ETag", strings.Trim(*resp.ETag, "\""))
	}
	if resp.LastModified != nil {
		w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))
	}

	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logger.Warn("stream object", zap.String("key", key), zap.Error(err))
	}
}

// @Summary Artifact metadata
// @Tags artifacts
// @Param artifactPath path string true "Artifact path (maps to S3 key with optional prefix)"
// @Success 200 {string} string "OK"
// @Failure 404 {string} string "Not Found"
// @Security BasicAuth
// @Router /{artifactPath} [head]
func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, key string) {
	resp, err := s.store.Head(r.Context(), key)
	if err != nil {
		if storage.IsNotFound(err) {
			if presp, found, perr := s.proxy.Head(r.Context(), key); perr != nil {
				s.writeError(w, "proxy head", perr)
				return
			} else if found {
				defer presp.Body.Close()
				if cl := presp.Header.Get("Content-Length"); cl != "" {
					w.Header().Set("Content-Length", cl)
				}
				if ct := presp.Header.Get("Content-Type"); ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				if lm := presp.Header.Get("Last-Modified"); lm != "" {
					w.Header().Set("Last-Modified", lm)
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			http.NotFound(w, r)
			return
		}
		s.writeError(w, "head object", err)
		return
	}

	if resp.ContentLength != nil && *resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(*resp.ContentLength, 10))
	}
	if resp.ContentType != nil {
		w.Header().Set("Content-Type", *resp.ContentType)
	}
	if resp.ETag != nil {
		w.Header().Set("ETag", strings.Trim(*resp.ETag, "\""))
	}
	if resp.LastModified != nil {
		w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))
	}

	w.WriteHeader(http.StatusOK)
}

// @Summary Upload artifact
// @Tags artifacts
// @Param artifactPath path string true "Artifact path (maps to S3 key with optional prefix)"
// @Accept application/octet-stream
// @Produce plain
// @Success 201 {string} string "Created"
// @Security BasicAuth
// @Router /{artifactPath} [put]
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	defer r.Body.Close()

	if r.ContentLength < 0 {
		http.Error(w, "Content-Length required", http.StatusLengthRequired)
		return
	}

	contentType := r.Header.Get("Content-Type")
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
