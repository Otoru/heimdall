package server

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap"
	"golang.org/x/net/html"
)

var proxyNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

const proxyConfigPrefix = "__proxycfg__/"

// sha1HexRe matches a bare SHA-1 digest. Upstream checksum files are not
// consistently formatted: some carry a trailing newline, some use the
// "<digest>  <filename>" form produced by sha1sum.
var sha1HexRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ErrChecksumMismatch is returned when the bytes fetched from upstream do not
// match the checksum upstream publishes for them. The artifact is discarded
// rather than cached: a poisoned cache entry is served forever and surfaces as
// an unrelated failure much later (a missing class at build time, say), while a
// failed request is simply retried.
var ErrChecksumMismatch = errors.New("upstream checksum mismatch")

type Proxy struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type ProxyStatusError struct {
	Code int
}

func (e ProxyStatusError) Error() string {
	return fmt.Sprintf("proxy fetch: status %d", e.Code)
}

// ProxyOptions tunes the reliability behaviour of a ProxyManager.
type ProxyOptions struct {
	// CacheTTL is how long the proxy definition list is reused before being
	// reloaded from storage.
	CacheTTL time.Duration
	// CacheStaleGrace is how long a stale definition list keeps being served
	// after a reload failure.
	CacheStaleGrace time.Duration
	// RetryAttempts is the total number of attempts for an upstream request.
	RetryAttempts int
	// VerifyChecksums enables validating fetched artifacts against the
	// checksum published upstream before caching them.
	VerifyChecksums bool
	// Timeout bounds a single upstream request.
	Timeout time.Duration

	// Metric hooks; all optional.
	OnCacheResult func(proxyCacheResult)
	OnRetry       func()
	OnFetch       func(proxy, result string)
}

func defaultProxyOptions() ProxyOptions {
	return ProxyOptions{
		CacheTTL:        defaultProxyCacheTTL,
		CacheStaleGrace: defaultProxyCacheStaleGrace,
		RetryAttempts:   defaultRetryAttempts,
		VerifyChecksums: true,
		Timeout:         60 * time.Second,
	}
}

type ProxyManager struct {
	store      Storage
	logger     *zap.Logger
	httpClient *http.Client
	cache      *proxyCache
	retry      retryPolicy
	verify     bool
	onFetch    func(proxy, result string)
}

func NewProxyManager(store Storage, logger *zap.Logger) *ProxyManager {
	return NewProxyManagerWithOptions(store, logger, defaultProxyOptions())
}

func NewProxyManagerWithOptions(store Storage, logger *zap.Logger, opts ProxyOptions) *ProxyManager {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.OnFetch == nil {
		opts.OnFetch = func(string, string) {}
	}
	return &ProxyManager{
		store:  store,
		logger: logger,
		httpClient: &http.Client{
			Timeout: opts.Timeout,
		},
		cache:   newProxyCache(opts.CacheTTL, opts.CacheStaleGrace, logger, opts.OnCacheResult),
		retry:   newRetryPolicy(opts.RetryAttempts, logger, opts.OnRetry),
		verify:  opts.VerifyChecksums,
		onFetch: opts.OnFetch,
	}
}

// List returns the configured proxies, served from an in-process cache.
//
// This used to hit object storage on every call, and every call site invoked it
// several times per artifact request. Worse, a proxy whose definition failed to
// load was skipped with only a warning, so a single transient storage error
// made an entire upstream disappear and the request 404 despite the artifact
// existing. Both behaviours are gone: loads are memoized, and a failed load
// fails the whole refresh instead of quietly shrinking the list.
func (p *ProxyManager) List(ctx context.Context) ([]Proxy, error) {
	return p.cache.get(ctx, p.loadAll)
}

// errMalformedProxyConfig marks a proxy definition whose bytes were read
// successfully but could not be decoded. Unlike a read failure, this is a
// persistent operator error: the definition will not decode on the next attempt
// either, so failing every request over it would turn one bad file into a total
// outage. It is skipped and logged instead.
var errMalformedProxyConfig = errors.New("malformed proxy config")

func (p *ProxyManager) loadAll(ctx context.Context) ([]Proxy, error) {
	entries, err := p.store.List(ctx, proxyConfigPrefix, 1000)
	if err != nil {
		return nil, fmt.Errorf("list proxy configs: %w", err)
	}

	var proxies []Proxy
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if !strings.HasSuffix(e.Path, ".json") {
			continue
		}
		cfg, err := p.load(ctx, e.Path)
		switch {
		case errors.Is(err, errMalformedProxyConfig):
			if p.logger != nil {
				p.logger.Warn("skipping malformed proxy config", zap.String("path", e.Path), zap.Error(err))
			}
			continue
		case err != nil:
			// A read failure is transient. Returning a partial list here is
			// what produced phantom 404s: one flaky GET dropped an entire
			// upstream, so artifacts already cached under it stopped
			// resolving. Fail the refresh instead and let the cache serve the
			// last known-good list.
			return nil, fmt.Errorf("load proxy %q: %w", e.Path, err)
		}
		proxies = append(proxies, cfg)
	}
	return proxies, nil
}

func (p *ProxyManager) load(ctx context.Context, cfgPath string) (Proxy, error) {
	resp, err := p.store.Get(ctx, cfgPath)
	if err != nil {
		return Proxy{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Proxy{}, err
	}
	var proxy Proxy
	if err := json.Unmarshal(body, &proxy); err != nil {
		return Proxy{}, fmt.Errorf("%w %q: %v", errMalformedProxyConfig, cfgPath, err)
	}
	return proxy, nil
}

func (p *ProxyManager) Add(ctx context.Context, proxy Proxy) error {
	proxy.Name = strings.TrimSpace(proxy.Name)
	proxy.URL = strings.TrimSpace(proxy.URL)

	if !proxyNameRe.MatchString(proxy.Name) {
		return fmt.Errorf("invalid name; only letters, digits, dot, underscore, dash")
	}
	if proxy.URL == "" {
		return fmt.Errorf("url is required")
	}

	data, err := json.Marshal(proxy)
	if err != nil {
		return err
	}
	cfgKey := path.Join(proxyConfigPrefix, proxy.Name+".json")
	if err := p.store.Put(ctx, cfgKey, strings.NewReader(string(data)), "application/json", int64(len(data))); err != nil {
		return err
	}
	p.cache.invalidate()
	return nil
}

func (p *ProxyManager) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if !proxyNameRe.MatchString(name) {
		return fmt.Errorf("invalid name")
	}
	base := path.Join(proxyConfigPrefix, name+".json")
	_ = p.store.Delete(ctx, base+".sha1")
	_ = p.store.Delete(ctx, base+".md5")
	if err := p.store.Delete(ctx, base); err != nil {
		return err
	}
	p.cache.invalidate()
	return nil
}

func (p *ProxyManager) Update(ctx context.Context, name string, proxy Proxy) error {
	proxy.Name = name
	return p.Add(ctx, proxy)
}

func (p *ProxyManager) FetchFromAny(ctx context.Context, artifactPath string) (string, bool, error) {
	proxies, err := p.List(ctx)
	if err != nil {
		return "", false, err
	}
	var lastStatus ProxyStatusError
	for _, pr := range proxies {
		key := path.Join(pr.Name, artifactPath)
		found, err := p.FetchAndCache(ctx, key)
		if err != nil {
			if se, ok := err.(ProxyStatusError); ok && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden || se.Code == http.StatusNotFound) {
				lastStatus = se
				continue
			}
			return "", false, err
		}
		if found {
			return key, true, nil
		}
	}
	if lastStatus.Code != 0 {
		return "", false, lastStatus
	}
	return "", false, nil
}

func (p *ProxyManager) HeadFromAny(ctx context.Context, artifactPath string) (*http.Response, bool, error) {
	proxies, err := p.List(ctx)
	if err != nil {
		return nil, false, err
	}
	var lastStatus ProxyStatusError
	for _, pr := range proxies {
		key := path.Join(pr.Name, artifactPath)
		resp, found, err := p.Head(ctx, key)
		if err != nil {
			if se, ok := err.(ProxyStatusError); ok && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden || se.Code == http.StatusNotFound) {
				lastStatus = se
				continue
			}
			return nil, false, err
		}
		if found {
			return resp, true, nil
		}
	}
	if lastStatus.Code != 0 {
		return nil, false, lastStatus
	}
	return nil, false, nil
}

func (p *ProxyManager) findByName(ctx context.Context, name string) (Proxy, bool, error) {
	list, err := p.List(ctx)
	if err != nil {
		return Proxy{}, false, err
	}
	for _, pr := range list {
		if pr.Name == name {
			return pr, true, nil
		}
	}
	return Proxy{}, false, nil
}

func splitProxyKey(key string) (proxyName, artifactPath string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(key, "/"), "/", 2)
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func isChecksumPath(artifactPath string) bool {
	lower := strings.ToLower(artifactPath)
	return strings.HasSuffix(lower, ".sha1") ||
		strings.HasSuffix(lower, ".md5") ||
		strings.HasSuffix(lower, ".sha256") ||
		strings.HasSuffix(lower, ".sha512") ||
		strings.HasSuffix(lower, ".asc")
}

// verifiablePath reports whether it is worth asking upstream for a checksum of
// this artifact. Checksum and signature files have none, and maven-metadata.xml
// is regenerated upstream often enough that its published checksum is routinely
// out of step with the document itself.
func verifiablePath(artifactPath string) bool {
	if isChecksumPath(artifactPath) {
		return false
	}
	return !strings.HasSuffix(strings.ToLower(path.Base(artifactPath)), "maven-metadata.xml")
}

func (p *ProxyManager) FetchAndCache(ctx context.Context, key string) (bool, error) {
	name, artifactPath, ok := splitProxyKey(key)
	if !ok {
		return false, nil
	}

	proxy, found, err := p.findByName(ctx, name)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	targetURL := strings.TrimSuffix(proxy.URL, "/") + "/" + artifactPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.retry.do(p.httpClient, req)
	if err != nil {
		p.onFetch(name, "error")
		return false, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		p.onFetch(name, "notfound")
		return false, nil
	case resp.StatusCode >= 300:
		p.onFetch(name, "error")
		return false, ProxyStatusError{Code: resp.StatusCode}
	}

	cached, err := p.cacheResponse(ctx, key, targetURL, resp, artifactPath)
	switch {
	case errors.Is(err, ErrChecksumMismatch):
		p.onFetch(name, "mismatch")
	case err != nil:
		p.onFetch(name, "error")
	default:
		p.onFetch(name, "fetched")
	}
	return cached, err
}

// upstreamSHA1 returns the SHA-1 digest upstream publishes for an artifact, if
// it publishes one. A missing or malformed checksum is not an error: plenty of
// artifacts have none, and refusing to cache those would be worse than not
// verifying them.
func (p *ProxyManager) upstreamSHA1(ctx context.Context, targetURL string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL+".sha1", nil)
	if err != nil {
		return "", false
	}
	resp, err := p.retry.do(p.httpClient, req)
	if err != nil {
		return "", false
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	// Checksum files are 40 bytes plus at most a filename; cap the read.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", false
	}
	sum := strings.ToLower(strings.TrimSpace(string(raw)))
	if idx := strings.IndexAny(sum, " \t\r\n"); idx > 0 {
		sum = sum[:idx]
	}
	if !sha1HexRe.MatchString(sum) {
		return "", false
	}
	return sum, true
}

func (p *ProxyManager) cacheResponse(ctx context.Context, key, targetURL string, resp *http.Response, artifactPath string) (bool, error) {
	tmp, err := os.CreateTemp("", "heimdall-proxy-*")
	if err != nil {
		return false, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	sha1h := sha1.New()
	md5h := md5.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sha1h, md5h), resp.Body); err != nil {
		return false, err
	}
	info, err := tmp.Stat()
	if err != nil {
		return false, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	gotSHA1 := hex.EncodeToString(sha1h.Sum(nil))

	if p.verify && verifiablePath(artifactPath) {
		if want, ok := p.upstreamSHA1(ctx, targetURL); ok && want != gotSHA1 {
			if p.logger != nil {
				p.logger.Warn("discarding artifact that does not match upstream checksum",
					zap.String("key", key),
					zap.String("expected", want),
					zap.String("actual", gotSHA1),
					zap.Int64("bytes", info.Size()),
				)
			}
			return false, fmt.Errorf("%w for %s: expected %s, got %s", ErrChecksumMismatch, key, want, gotSHA1)
		}
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	// Checksums are written before the artifact. Readers look the artifact up
	// first, so this ordering guarantees that a visible artifact always has a
	// matching checksum beside it. The reverse order leaves a window in which
	// the artifact is served with a stale or absent checksum, which Maven
	// reports as a corrupt download.
	if !isChecksumPath(artifactPath) {
		if err := p.storeChecksums(ctx, key, gotSHA1, md5h); err != nil {
			return false, err
		}
	}

	if err := p.store.Put(ctx, key, tmp, ct, info.Size()); err != nil {
		return false, err
	}

	return true, nil
}

func (p *ProxyManager) storeChecksums(ctx context.Context, key, sha1sum string, md5h hash.Hash) error {
	md5sum := hex.EncodeToString(md5h.Sum(nil))
	if err := p.store.Put(ctx, key+".sha1", strings.NewReader(sha1sum), "text/plain", int64(len(sha1sum))); err != nil {
		return err
	}
	return p.store.Put(ctx, key+".md5", strings.NewReader(md5sum), "text/plain", int64(len(md5sum)))
}

func splitListKey(key string) (name, artifactPath string) {
	trimmed := strings.TrimPrefix(key, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	name = parts[0]
	if len(parts) == 2 {
		artifactPath = parts[1]
	}
	return
}

func hrefToEntry(href string) (storage.Entry, bool) {
	if href == "../" || href == "" {
		return storage.Entry{}, false
	}
	u, err := url.Parse(href)
	if err != nil {
		return storage.Entry{}, false
	}
	raw := strings.TrimSpace(u.Path)
	if raw == "" {
		return storage.Entry{}, false
	}
	norm := strings.TrimPrefix(raw, "/")
	isDir := strings.HasSuffix(norm, "/")
	norm = strings.TrimSuffix(norm, "/")
	if norm == "" || strings.Contains(norm, "/") {
		return storage.Entry{}, false
	}
	entryName := norm
	if isDir {
		entryName += "/"
	}
	etype := "file"
	if isDir {
		etype = "dir"
	}
	return storage.Entry{Name: entryName, Path: path.Join(entryName, ""), Type: etype}, true
}

func nodeToEntry(n *html.Node) (storage.Entry, bool) {
	if n.Type != html.ElementNode || n.Data != "a" {
		return storage.Entry{}, false
	}
	for _, attr := range n.Attr {
		if attr.Key == "href" {
			return hrefToEntry(attr.Val)
		}
	}
	return storage.Entry{}, false
}

func parseHTMLEntries(root *html.Node, limit int32) []storage.Entry {
	var entries []storage.Entry
	stack := []*html.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if limit > 0 && int32(len(entries)) >= limit {
			break
		}
		if e, ok := nodeToEntry(n); ok {
			entries = append(entries, e)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			stack = append(stack, c)
		}
	}
	return entries
}

func (p *ProxyManager) ListPath(ctx context.Context, key string, limit int32) ([]storage.Entry, bool, error) {
	name, artifactPath := splitListKey(key)
	if name == "" {
		return nil, false, nil
	}

	proxy, found, err := p.findByName(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	target := strings.TrimSuffix(proxy.URL, "/") + "/" + artifactPath
	if !strings.HasSuffix(target, "/") {
		target += "/"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, true, err
	}
	resp, err := p.retry.do(p.httpClient, req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return []storage.Entry{}, true, nil
	case resp.StatusCode >= 300:
		return nil, true, ProxyStatusError{Code: resp.StatusCode}
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, true, err
	}

	return parseHTMLEntries(doc, limit), true, nil
}

func (p *ProxyManager) Head(ctx context.Context, key string) (*http.Response, bool, error) {
	name, artifactPath, ok := splitProxyKey(key)
	if !ok {
		return nil, false, nil
	}
	proxy, found, err := p.findByName(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	url := strings.TrimSuffix(proxy.URL, "/") + "/" + artifactPath
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := p.retry.do(p.httpClient, req)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, false, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, false, ProxyStatusError{Code: resp.StatusCode}
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, false, fmt.Errorf("proxy head: status %d", resp.StatusCode)
	}
	return resp, true, nil
}
