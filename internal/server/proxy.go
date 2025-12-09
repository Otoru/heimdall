package server

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

type ProxyManager struct {
	store      Storage
	logger     *zap.Logger
	httpClient *http.Client
}

func NewProxyManager(store Storage, logger *zap.Logger) *ProxyManager {
	return &ProxyManager{
		store:  store,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (p *ProxyManager) List(ctx context.Context) ([]Proxy, error) {
	entries, err := p.store.List(ctx, proxyConfigPrefix, 1000)
	if err != nil {
		return nil, err
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
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("load proxy", zap.String("path", e.Path), zap.Error(err))
			}
			continue
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
		return Proxy{}, err
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
	return p.store.Put(ctx, cfgKey, strings.NewReader(string(data)), "application/json", int64(len(data)))
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
	return p.store.Delete(ctx, base)
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
			if se, ok := err.(ProxyStatusError); ok && (se.Code == http.StatusNotFound || se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden) {
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
			if se, ok := err.(ProxyStatusError); ok && (se.Code == http.StatusNotFound || se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden) {
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

func (p *ProxyManager) FetchAndCache(ctx context.Context, key string) (bool, error) {
	name, artifactPath, ok := splitProxyKey(key)
	if !ok {
		return false, nil
	}

	isChecksum := strings.HasSuffix(strings.ToLower(artifactPath), ".sha1") || strings.HasSuffix(strings.ToLower(artifactPath), ".md5")

	proxy, found, err := p.findByName(ctx, name)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	url := strings.TrimSuffix(proxy.URL, "/") + "/" + artifactPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		return false, ProxyStatusError{Code: resp.StatusCode}
	}

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
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if err := p.store.Put(ctx, key, tmp, contentType, info.Size()); err != nil {
		return false, err
	}

	if !isChecksum {
		sha1sum := hex.EncodeToString(sha1h.Sum(nil))
		md5sum := hex.EncodeToString(md5h.Sum(nil))
		if err := p.store.Put(ctx, key+".sha1", strings.NewReader(sha1sum), "text/plain", int64(len(sha1sum))); err != nil {
			return false, err
		}
		if err := p.store.Put(ctx, key+".md5", strings.NewReader(md5sum), "text/plain", int64(len(md5sum))); err != nil {
			return false, err
		}
	}

	return true, nil
}

func (p *ProxyManager) ListPath(ctx context.Context, key string, limit int32) ([]storage.Entry, bool, error) {
	trimmed := strings.TrimPrefix(key, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	name := parts[0]
	artifactPath := ""
	if name == "" {
		return nil, false, nil
	}
	if len(parts) == 2 {
		artifactPath = parts[1]
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
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []storage.Entry{}, true, nil
	}
	if resp.StatusCode >= 300 {
		return nil, true, fmt.Errorf("proxy list: status %d", resp.StatusCode)
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, true, err
	}

	var entries []storage.Entry
	var walker func(*html.Node)
	walker = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key != "href" {
					continue
				}
				href := attr.Val
				if href == "../" || href == "" {
					continue
				}
				u, err := url.Parse(href)
				if err != nil {
					continue
				}
				raw := strings.TrimSpace(u.Path)
				if raw == "" {
					continue
				}
				norm := strings.TrimPrefix(raw, "/")
				isDir := strings.HasSuffix(norm, "/")
				norm = strings.TrimSuffix(norm, "/")
				if norm == "" {
					continue
				}
				if strings.Contains(norm, "/") {
					// skip nested segments; we only want immediate children
					continue
				}
				name := norm
				if isDir {
					name += "/"
				}
				etype := "file"
				if isDir {
					etype = "dir"
				}
				entries = append(entries, storage.Entry{
					Name: name,
					Path: path.Join(name, ""),
					Type: etype,
				})
				if limit > 0 && int32(len(entries)) >= limit {
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if limit > 0 && int32(len(entries)) >= limit {
				return
			}
			walker(c)
		}
	}
	walker(doc)

	return entries, true, nil
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
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, false, nil
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, false, fmt.Errorf("proxy head: status %d", resp.StatusCode)
	}
	return resp, true, nil
}
