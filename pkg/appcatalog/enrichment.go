package appcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultEnrichCacheTTL    = 24 * time.Hour
	defaultEnrichCacheMax    = 512
	defaultEnrichHTTPTimeout = 5 * time.Second
	maxEnrichResponseBytes   = 256 << 10
	maxEnrichURLLength       = 512

	EnrichSourceCatalog  = "catalog"
	EnrichSourceTinyFish = "tinyfish"
	EnrichSourceExa      = "exa"
	EnrichSourceParallel = "parallel"
	EnrichSourceFallback = "fallback"

	contextDevLogoEndpoint = "https://logos.context.dev/"
	defaultTinyFishURL     = "https://api.tinyfish.io/v1/search"
	defaultExaURL          = "https://api.exa.ai/search"
	defaultParallelURL     = "https://api.parallel.ai/v1beta/search"
)

// EnrichRequest identifies an application to resolve metadata for.
type EnrichRequest struct {
	Query          string            `json:"query,omitempty"`
	Image          string            `json:"image,omitempty"`
	ContainerName  string            `json:"container_name,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	AllowConnected bool              `json:"allow_connected,omitempty"`
}

// EnrichedMetadata is the resolved proposal metadata for an application.
type EnrichedMetadata struct {
	CatalogID   string `json:"catalog_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Category    string `json:"category,omitempty"`
	BrandDomain string `json:"brand_domain,omitempty"`
	VendorName  string `json:"vendor_name,omitempty"`
	HomepageURL string `json:"homepage_url,omitempty"`
	DocsURL     string `json:"docs_url,omitempty"`
	SourceRepo  string `json:"source_repo,omitempty"`
	IconURL     string `json:"icon_url,omitempty"`
	Logo        *Logo  `json:"logo,omitempty"`
	LogoRemoved bool   `json:"logo_removed,omitempty"`
	WebPort     int    `json:"web_port,omitempty"`
	Source      string `json:"source"`
}

// SearchResult is one public web search hit returned by a Tier-1 provider.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
	Domain  string
}

// SearchProvider is a pluggable Tier-1 Connected-mode lookup backend.
type SearchProvider interface {
	Name() string
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// EnricherConfig configures an Enricher.
type EnricherConfig struct {
	Providers        []SearchProvider
	LogoLinkClientID string
	LocalOnly        bool
	CacheTTL         time.Duration
	CacheMaxEntries  int
	Now              func() time.Time
}

type enrichCacheEntry struct {
	value     EnrichedMetadata
	expiresAt time.Time
	storedAt  time.Time
}

// Enricher resolves application metadata in two tiers:
//   - Tier 0 (instant, offline): embedded/released appcatalog + mirrored icons.
//   - Tier 1 (optional Connected-mode): pluggable web search (TinyFish, Exa,
//     Parallel) and Context.dev Logo Link fallback when no mirrored icon exists.
//
// In local-only/offline mode Tier 1 and Context.dev are never called.
type Enricher struct {
	providers        []SearchProvider
	logoLinkClientID string
	localOnly        bool
	cacheTTL         time.Duration
	cacheMax         int
	now              func() time.Time

	mu    sync.RWMutex
	cache map[string]enrichCacheEntry
}

// NewEnricher creates an Enricher from cfg.
func NewEnricher(cfg EnricherConfig) *Enricher {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = defaultEnrichCacheTTL
	}
	maxEntries := cfg.CacheMaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultEnrichCacheMax
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Enricher{
		providers:        append([]SearchProvider(nil), cfg.Providers...),
		logoLinkClientID: strings.TrimSpace(cfg.LogoLinkClientID),
		localOnly:        cfg.LocalOnly,
		cacheTTL:         ttl,
		cacheMax:         maxEntries,
		now:              now,
		cache:            make(map[string]enrichCacheEntry),
	}
}

func newEnrichHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultEnrichHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// NewDefaultEnricher builds an Enricher wired from environment variables.
// When HOMELAB_LOCAL_ONLY or HOMELAB_OFFLINE is truthy (or
// HOMELAB_ENRICH_CONNECTED is explicitly false), Connected lookups are disabled.
func NewDefaultEnricher() *Enricher {
	localOnly := envBool("HOMELAB_LOCAL_ONLY") || envBool("HOMELAB_OFFLINE")
	if raw := strings.TrimSpace(os.Getenv("HOMELAB_ENRICH_CONNECTED")); raw != "" && !parseTruthy(raw) {
		localOnly = true
	}
	var providers []SearchProvider
	if !localOnly {
		httpClient := newEnrichHTTPClient()
		if key := firstNonEmpty(os.Getenv("HOMELAB_ENRICH_TINYFISH_API_KEY"), os.Getenv("TINYFISH_API_KEY")); key != "" {
			tinyFishURL := defaultTinyFishURL
			if rawURL := strings.TrimSpace(os.Getenv("HOMELAB_ENRICH_TINYFISH_URL")); rawURL != "" {
				if safeURL, err := ValidatePublicHTTPSURL(rawURL); err == nil {
					tinyFishURL = safeURL
				}
			}
			providers = append(providers, &TinyFishProvider{
				APIKey:  key,
				BaseURL: tinyFishURL,
				Client:  httpClient,
			})
		}
		if key := strings.TrimSpace(os.Getenv("HOMELAB_ENRICH_EXA_API_KEY")); key != "" {
			providers = append(providers, &ExaProvider{
				APIKey:  key,
				BaseURL: defaultExaURL,
				Client:  httpClient,
			})
		}
		if key := strings.TrimSpace(os.Getenv("HOMELAB_ENRICH_PARALLEL_API_KEY")); key != "" {
			providers = append(providers, &ParallelProvider{
				APIKey:  key,
				BaseURL: defaultParallelURL,
				Client:  httpClient,
			})
		}
	}
	return NewEnricher(EnricherConfig{
		Providers:        providers,
		LogoLinkClientID: strings.TrimSpace(os.Getenv("HOMELAB_LOGO_LINK_CLIENT_ID")),
		LocalOnly:        localOnly,
	})
}

// ConnectedEnabled reports whether Connected-mode Tier-1 enrichment is allowed.
func (e *Enricher) ConnectedEnabled() bool {
	if e == nil || e.localOnly {
		return false
	}
	return len(e.providers) > 0 || e.logoLinkClientID != ""
}

// Enrich resolves metadata for req. It always succeeds fail-closed, falling back
// to Tier-0 catalog data or a safe display name when external lookups are
// disabled, unconfigured, or fail.
func (e *Enricher) Enrich(ctx context.Context, req EnrichRequest) EnrichedMetadata {
	if e == nil {
		return enrichTier0(req)
	}
	allowConnected := req.AllowConnected && !e.localOnly
	cacheKey := enrichCacheKey(req, allowConnected)
	if cached, ok := e.getCached(cacheKey); ok {
		return cached
	}

	meta := enrichTier0(req)
	if allowConnected {
		e.applyTier1(ctx, req, &meta)
	}
	e.putCached(cacheKey, meta)
	return cloneEnrichedMetadata(meta)
}

func enrichTier0(req EnrichRequest) EnrichedMetadata {
	entry := matchForEnrich(req)
	if entry == nil {
		name := firstNonEmpty(
			LabelDisplayName(req.Labels),
			strings.TrimPrefix(strings.TrimSpace(req.ContainerName), "/"),
			strings.TrimSpace(req.Query),
			imageBaseName(req.Image),
		)
		return EnrichedMetadata{
			DisplayName: name,
			Category:    "unknown",
			IconURL:     genericIconURL(loadGeneratedTable()),
			Source:      EnrichSourceFallback,
		}
	}

	meta := EnrichedMetadata{
		CatalogID:   entry.ID,
		DisplayName: entry.DisplayName,
		Category:    entry.Category,
		BrandDomain: entry.BrandDomain,
		HomepageURL: sanitizePublicHTTPSURL(entry.HomepageURL),
		IconURL:     entry.IconURL,
		Logo:        cloneLogo(entry.Logo),
		LogoRemoved: entry.LogoRemoved,
		WebPort:     entry.WebPort,
		Source:      EnrichSourceCatalog,
	}
	if entry.Vendor != nil {
		meta.VendorName = strings.TrimSpace(entry.Vendor.Name)
		meta.DocsURL = sanitizePublicHTTPSURL(entry.Vendor.Docs)
		if meta.HomepageURL == "" {
			meta.HomepageURL = sanitizePublicHTTPSURL(entry.Vendor.Website)
		}
	}
	if app, ok := loadGeneratedTable().apps[entry.ID]; ok && app != nil {
		if repo := strings.TrimSpace(app.SourceRepo); repo != "" {
			meta.SourceRepo = normalizeSourceRepoURL(repo)
		}
	}
	if meta.BrandDomain == "" && meta.HomepageURL != "" {
		meta.BrandDomain = homepageDomain(meta.HomepageURL)
	}
	return meta
}

func matchForEnrich(req EnrichRequest) *Entry {
	subject := Subject{
		Name:   strings.TrimSpace(req.ContainerName),
		Image:  strings.TrimSpace(req.Image),
		Labels: RecognitionLabels(req.Labels),
	}
	if entry := Match(subject); entry != nil {
		return entry
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil
	}
	if entry := lookupKnown(query, false); entry != nil {
		return entry
	}
	return Match(Subject{Name: query, Image: query})
}

func (e *Enricher) applyTier1(ctx context.Context, req EnrichRequest, meta *EnrichedMetadata) {
	needsSearch := meta.Source == EnrichSourceFallback || meta.HomepageURL == "" || meta.DocsURL == ""
	if needsSearch && len(e.providers) > 0 {
		searchQuery := buildEnrichSearchQuery(req, meta)
		if searchQuery != "" {
			for _, provider := range e.providers {
				if provider == nil {
					continue
				}
				results, err := provider.Search(ctx, searchQuery)
				if err != nil || len(results) == 0 {
					continue
				}
				if mergeSearchResults(meta, results, provider.Name()) {
					break
				}
			}
		}
	}
	// Context.dev Logo Link fallback: only when Connected mode is active,
	// brand_domain is known, no mirrored icon exists, and LogoRemoved is false.
	if !e.localOnly && e.logoLinkClientID != "" && meta.BrandDomain != "" && meta.Logo == nil && !meta.LogoRemoved {
		if logoURL := ContextDevLogoURL(meta.BrandDomain, e.logoLinkClientID); logoURL != "" {
			meta.IconURL = logoURL
		}
	}
}

func buildEnrichSearchQuery(req EnrichRequest, meta *EnrichedMetadata) string {
	base := firstNonEmpty(
		strings.TrimSpace(req.Query),
		strings.TrimSpace(meta.DisplayName),
		imageBaseName(req.Image),
		strings.TrimPrefix(strings.TrimSpace(req.ContainerName), "/"),
	)
	if base == "" {
		return ""
	}
	if len(base) > 120 {
		base = base[:120]
	}
	return base + " self-hosted docker official documentation homepage"
}

func mergeSearchResults(meta *EnrichedMetadata, results []SearchResult, providerName string) bool {
	changed := false
	for _, hit := range results {
		safeURL, err := ValidatePublicHTTPSURL(hit.URL)
		if err != nil {
			continue
		}
		parsed, _ := url.Parse(safeURL)
		host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
		pathLower := strings.ToLower(parsed.Path)

		if meta.SourceRepo == "" && isSourceRepoHost(host) {
			meta.SourceRepo = safeURL
			changed = true
		}
		if meta.DocsURL == "" && (strings.HasPrefix(host, "docs.") || strings.Contains(pathLower, "/docs") || strings.Contains(pathLower, "/documentation")) {
			meta.DocsURL = safeURL
			changed = true
		}
		if meta.HomepageURL == "" && !isSourceRepoHost(host) && !isAggregatorHost(host) {
			meta.HomepageURL = safeURL
			if meta.BrandDomain == "" && isValidPublicDomain(host) {
				meta.BrandDomain = host
			}
			changed = true
		}
		if meta.VendorName == "" && hit.Title != "" && !isAggregatorHost(host) {
			if title := cleanSearchTitle(hit.Title); title != "" {
				meta.VendorName = title
				changed = true
			}
		}
	}
	if meta.HomepageURL == "" && meta.SourceRepo != "" {
		meta.HomepageURL = meta.SourceRepo
		changed = true
	}
	if changed && meta.Source == EnrichSourceFallback {
		meta.Source = providerName
	}
	return changed
}

// ValidatePublicHTTPSURL enforces strict SSRF/URL validation for external
// metadata links: https:// only, no credentials, public multi-label hostname,
// no IP literals or private/local suffixes.
func ValidatePublicHTTPSURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxEnrichURLLength {
		return "", errors.New("url is empty or exceeds maximum length")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("only https URLs are permitted")
	}
	if parsed.User != nil {
		return "", errors.New("URL credentials are not permitted")
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if !isValidPublicDomain(host) {
		return "", errors.New("URL host must be a public domain name")
	}
	return parsed.String(), nil
}

func sanitizePublicHTTPSURL(raw string) string {
	safe, err := ValidatePublicHTTPSURL(raw)
	if err != nil {
		return ""
	}
	return safe
}

func isValidPublicDomain(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	if host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".lan") ||
		strings.HasSuffix(host, ".home.arpa") {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			valid := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || (ch == '-' && i > 0 && i < len(label)-1)
			if !valid {
				return false
			}
		}
	}
	return true
}

// ContextDevLogoURL builds the Context.dev Logo Link URL for a validated
// public brand domain and client ID. Returns "" if inputs are invalid.
func ContextDevLogoURL(domain, clientID string) string {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "www.")
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || !isValidPublicDomain(domain) {
		return ""
	}
	q := url.Values{}
	q.Set("domain", domain)
	q.Set("client_id", clientID)
	return contextDevLogoEndpoint + "?" + q.Encode()
}

func normalizeSourceRepoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if strings.HasPrefix(repo, "https://") {
		return sanitizePublicHTTPSURL(repo)
	}
	if strings.Count(repo, "/") == 1 && !strings.ContainsAny(repo, " \t\r\n?#") {
		return sanitizePublicHTTPSURL("https://github.com/" + repo)
	}
	return ""
}

func imageBaseName(image string) string {
	repo := ParseImage(image).Repo
	if repo == "" {
		return ""
	}
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

func isSourceRepoHost(host string) bool {
	return host == "github.com" || host == "gitlab.com" || host == "codeberg.org"
}

func isAggregatorHost(host string) bool {
	switch host {
	case "hub.docker.com", "docker.com", "reddit.com", "wikipedia.org", "en.wikipedia.org", "youtube.com":
		return true
	}
	return false
}

func cleanSearchTitle(title string) string {
	title = strings.TrimSpace(title)
	for _, sep := range []string{" - ", " | ", " · ", " — ", ": "} {
		if before, _, ok := strings.Cut(title, sep); ok && len(strings.TrimSpace(before)) >= 2 {
			title = strings.TrimSpace(before)
		}
	}
	if len(title) > 80 {
		title = title[:80]
	}
	return title
}

func enrichCacheKey(req EnrichRequest, allowConnected bool) string {
	mode := "offline"
	if allowConnected {
		mode = "connected"
	}
	return strings.Join([]string{
		mode,
		strings.ToLower(strings.TrimSpace(req.Query)),
		strings.ToLower(strings.TrimSpace(req.Image)),
		strings.ToLower(strings.TrimSpace(req.ContainerName)),
		strings.ToLower(strings.TrimSpace(LabelDisplayName(req.Labels))),
	}, "\x00")
}

func (e *Enricher) getCached(key string) (EnrichedMetadata, bool) {
	now := e.now()
	e.mu.RLock()
	entry, ok := e.cache[key]
	e.mu.RUnlock()
	if !ok || !now.Before(entry.expiresAt) {
		return EnrichedMetadata{}, false
	}
	return cloneEnrichedMetadata(entry.value), true
}

func (e *Enricher) putCached(key string, value EnrichedMetadata) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.cache) >= e.cacheMax {
		for k, v := range e.cache {
			if !now.Before(v.expiresAt) {
				delete(e.cache, k)
			}
		}
		if len(e.cache) >= e.cacheMax {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range e.cache {
				if oldestKey == "" || v.storedAt.Before(oldestTime) {
					oldestKey, oldestTime = k, v.storedAt
				}
			}
			if oldestKey != "" {
				delete(e.cache, oldestKey)
			}
		}
	}
	e.cache[key] = enrichCacheEntry{
		value:     cloneEnrichedMetadata(value),
		expiresAt: now.Add(e.cacheTTL),
		storedAt:  now,
	}
}

func cloneEnrichedMetadata(m EnrichedMetadata) EnrichedMetadata {
	m.Logo = cloneLogo(m.Logo)
	return m
}

func cloneLogo(l *Logo) *Logo {
	if l == nil {
		return nil
	}
	copied := *l
	return &copied
}

func envBool(name string) bool {
	return parseTruthy(os.Getenv(name))
}

func parseTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// --- Tier-1 SearchProvider adapters ---

// TinyFishProvider queries Kombify's first-party TinyFish search lane.
type TinyFishProvider struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func (p *TinyFishProvider) Name() string { return EnrichSourceTinyFish }

func (p *TinyFishProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if strings.TrimSpace(p.APIKey) == "" {
		return nil, errors.New("tinyfish api key is not configured")
	}
	endpoint := firstNonEmpty(p.BaseURL, defaultTinyFishURL)
	body, err := json.Marshal(map[string]any{
		"query":       query,
		"max_results": 5,
	})
	if err != nil {
		return nil, err
	}
	raw, err := doEnrichPOST(ctx, p.Client, endpoint, body, map[string]string{
		"Authorization": "Bearer " + p.APIKey,
		"X-API-Key":     p.APIKey,
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			Text    string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: firstNonEmpty(r.Snippet, r.Text),
		})
	}
	return out, nil
}

// ExaProvider queries the Exa search API (https://api.exa.ai/search).
type ExaProvider struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func (p *ExaProvider) Name() string { return EnrichSourceExa }

func (p *ExaProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if strings.TrimSpace(p.APIKey) == "" {
		return nil, errors.New("exa api key is not configured")
	}
	endpoint := firstNonEmpty(p.BaseURL, defaultExaURL)
	body, err := json.Marshal(map[string]any{
		"query":      query,
		"numResults": 5,
		"type":       "auto",
	})
	if err != nil {
		return nil, err
	}
	raw, err := doEnrichPOST(ctx, p.Client, endpoint, body, map[string]string{
		"x-api-key": p.APIKey,
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Text,
		})
	}
	return out, nil
}

// ParallelProvider queries the Parallel search API (https://api.parallel.ai/v1beta/search).
type ParallelProvider struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func (p *ParallelProvider) Name() string { return EnrichSourceParallel }

func (p *ParallelProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if strings.TrimSpace(p.APIKey) == "" {
		return nil, errors.New("parallel api key is not configured")
	}
	endpoint := firstNonEmpty(p.BaseURL, defaultParallelURL)
	body, err := json.Marshal(map[string]any{
		"objective":   query,
		"max_results": 5,
	})
	if err != nil {
		return nil, err
	}
	raw, err := doEnrichPOST(ctx, p.Client, endpoint, body, map[string]string{
		"x-api-key": p.APIKey,
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []struct {
			Title    string   `json:"title"`
			URL      string   `json:"url"`
			Excerpts []string `json:"excerpts"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(payload.Results))
	for _, r := range payload.Results {
		snippet := ""
		if len(r.Excerpts) > 0 {
			snippet = r.Excerpts[0]
		}
		out = append(out, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: snippet,
		})
	}
	return out, nil
}

func doEnrichPOST(ctx context.Context, client *http.Client, endpoint string, body []byte, headers map[string]string) ([]byte, error) {
	if client == nil {
		client = newEnrichHTTPClient()
	}
	reqCtx, cancel := context.WithTimeout(ctx, defaultEnrichHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("enrichment provider returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxEnrichResponseBytes))
}
