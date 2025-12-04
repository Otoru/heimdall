package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
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
}

type Server struct {
	store   Storage
	logger  *zap.Logger
	metrics *metrics.Registry
	user    string
	pass    string
}

func New(store Storage, logger *zap.Logger, m *metrics.Registry, user, pass string) *Server {
	return &Server{
		store:   store,
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
			http.NotFound(w, r)
			return
		}
		s.writeError(w, "fetch object", err)
		return
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

	err = s.store.Put(r.Context(), key, tmp, contentType, r.ContentLength)
	if err != nil {
		s.writeError(w, "store object", err)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (s *Server) writeError(w http.ResponseWriter, action string, err error) {
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
