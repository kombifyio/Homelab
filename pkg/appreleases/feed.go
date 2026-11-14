// Package appreleases reads the StackKits application release feed
// (`stackkit.application-releases/v1`, StackKits schemas/
// stackkit-application-releases-v1.schema.json): per StackKits-managed
// application the newer upstream versions of its entry image within the
// track, whether the catalog admitted (tested) each one, and the known
// advisories of the upstream project.
//
// The feed is an observation, not a release. Techstack uses it only after
// the published digest receipt names exactly the bytes it read and only while
// the feed is younger than MaxFeedAge. Anything else is "not verified", never
// "up to date" or "no known vulnerabilities".
package appreleases

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/techstack/pkg/appcatalog"
	"github.com/kombifyio/techstack/pkg/fulllocal"
)

const (
	// FeedURL is the newest published feed; ReceiptURL names its digest.
	FeedURL    = "https://assets.kombify.io/stackkits/applications/latest.json"
	ReceiptURL = "https://assets.kombify.io/stackkits/applications/latest.receipt.json"

	// SchemaVersion is the only feed version this reader admits.
	SchemaVersion = "stackkit.application-releases/v1"
	// ReceiptSchemaVersion is the only receipt version this reader admits.
	ReceiptSchemaVersion = "stackkit.application-releases-receipt/v1"

	// MaxFeedAge is how old a feed may be and still count as verified.
	MaxFeedAge = 48 * time.Hour

	refreshInterval = 15 * time.Minute
	failureInterval = 5 * time.Minute
	requestTimeout  = 10 * time.Second
	maxFeedBytes    = 8 << 20
	maxReceiptBytes = 64 << 10
)

// Reason codes of a feed that cannot be used.
const (
	ReasonFeedPending     = "feed_pending"
	ReasonFeedUnreachable = "feed_unreachable"
	ReasonFeedInvalid     = "feed_invalid"
	ReasonReceiptMismatch = "receipt_mismatch"
	ReasonFeedStale       = "feed_stale"
	ReasonFeedDisabled    = "feed_disabled"
)

// Feed is the decoded feed. Unknown fields are tolerated: the producer may
// add per-candidate release notes or publish dates.
type Feed struct {
	SchemaVersion string        `json:"schemaVersion"`
	GeneratedAt   time.Time     `json:"generatedAt"`
	Applications  []Application `json:"applications"`
}

// Application is one watched application entry image.
type Application struct {
	Module        string      `json:"module"`
	CatalogModule string      `json:"catalogModule,omitempty"`
	Service       string      `json:"service"`
	Image         string      `json:"image"`
	Track         string      `json:"track"`
	Current       Current     `json:"current"`
	Status        string      `json:"status"`
	ObservedAt    time.Time   `json:"observedAt"`
	Evidence      []Evidence  `json:"evidence"`
	Candidates    []Candidate `json:"candidates"`
	Advisories    []Advisory  `json:"advisories"`
}

// Current is the catalog pin the feed observed from.
type Current struct {
	Version string `json:"version"`
	Ref     string `json:"ref"`
	Digest  string `json:"digest,omitempty"`
}

// Evidence is one source observation of an application.
type Evidence struct {
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observedAt"`
}

// Candidate is one newer in-track version of the entry image.
type Candidate struct {
	Version         string            `json:"version"`
	Ref             string            `json:"ref"`
	Digest          string            `json:"digest"`
	Platforms       map[string]string `json:"platforms"`
	Tested          bool              `json:"tested"`
	ReleaseNotesURL string            `json:"releaseNotesUrl,omitempty"`
	PublishedAt     string            `json:"publishedAt,omitempty"`
}

// Advisory is one upstream security advisory.
type Advisory struct {
	ID             string   `json:"id"`
	Aliases        []string `json:"aliases,omitempty"`
	Summary        string   `json:"summary,omitempty"`
	Severity       string   `json:"severity"`
	CVSS           *CVSS    `json:"cvss,omitempty"`
	AffectedRange  string   `json:"affectedRange,omitempty"`
	FixedVersion   string   `json:"fixedVersion,omitempty"`
	AffectsCurrent *bool    `json:"affectsCurrent"`
	URL            string   `json:"url"`
	PublishedAt    string   `json:"publishedAt,omitempty"`
}

// CVSS is the advisory score when the source states one.
type CVSS struct {
	Score  *float64 `json:"score,omitempty"`
	Vector string   `json:"vector,omitempty"`
}

type receipt struct {
	SchemaVersion string `json:"schemaVersion"`
	Feed          struct {
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"feed"`
}

// Status is what the reader holds now. A Status that is not Verified carries
// no applications.
type Status struct {
	Verified    bool
	Reason      string
	GeneratedAt time.Time
	FetchedAt   time.Time
	feed        *Feed
	index       map[string]int
}

// Reader keeps the feed fresh in the background. It never blocks a caller on
// the network: the first read starts a fetch and reports feed_pending.
type Reader struct {
	client     *http.Client
	now        func() time.Time
	feedURL    string
	receiptURL string

	mu       sync.Mutex
	loading  bool
	next     time.Time
	etag     string
	body     []byte
	fetched  time.Time
	feed     *Feed
	index    map[string]int
	failure  string
	verified bool
}

// NewReader returns the reader of the published feed. In Full Local it
// returns nil: the edition sends nothing to the public asset host, so every
// fact reports feed_disabled.
func NewReader() *Reader {
	if fulllocal.Active() {
		return nil
	}
	return &Reader{
		client: &http.Client{Timeout: requestTimeout, CheckRedirect: sameHostRedirect}, now: func() time.Time { return time.Now().UTC() },
		feedURL: FeedURL, receiptURL: ReceiptURL, failure: ReasonFeedPending,
	}
}

// Status returns the current feed state and starts a refresh when one is due.
func (r *Reader) Status() Status {
	if r == nil {
		return Status{Reason: ReasonFeedDisabled}
	}
	now := r.now()
	r.mu.Lock()
	if !r.loading && !now.Before(r.next) {
		r.loading = true
		go r.refresh()
	}
	status := Status{Reason: r.failure, FetchedAt: r.fetched}
	if r.verified && r.feed != nil {
		status.GeneratedAt = r.feed.GeneratedAt
		if now.Sub(r.feed.GeneratedAt) > MaxFeedAge {
			status.Reason = ReasonFeedStale
		} else {
			status.Verified, status.Reason, status.feed, status.index = true, "", r.feed, r.index
		}
	}
	r.mu.Unlock()
	return status
}

// sameHostRedirect follows a redirect only to the asset host itself.
func sameHostRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 3 || len(via) == 0 || request.URL.Scheme != "https" || request.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}

func (r *Reader) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*requestTimeout)
	defer cancel()
	r.mu.Lock()
	etag, previous := r.etag, r.body
	r.mu.Unlock()

	body, nextETag, err := r.fetchFeed(ctx, etag, previous)
	var (
		feed   *Feed
		index  map[string]int
		reason string
	)
	if err != nil {
		reason = ReasonFeedUnreachable
	} else if want, receiptErr := r.fetchReceipt(ctx); receiptErr != nil {
		reason = ReasonFeedUnreachable
	} else if sum := sha256.Sum256(body); want != "sha256:"+hex.EncodeToString(sum[:]) {
		// latest.json is replaced before its receipt: a mismatch is either
		// that short window or a tampered copy, and both stay unverified.
		reason = ReasonReceiptMismatch
	} else if feed, index, err = decodeFeed(body); err != nil {
		reason = ReasonFeedInvalid
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.loading = false
	r.fetched = r.now()
	if reason != "" {
		r.failure, r.verified, r.feed, r.index = reason, false, nil, nil
		r.next = r.fetched.Add(failureInterval)
		if body != nil {
			r.etag, r.body = nextETag, body
		}
		return
	}
	r.failure, r.verified, r.feed, r.index = "", true, feed, index
	r.etag, r.body = nextETag, body
	r.next = r.fetched.Add(refreshInterval)
}

// fetchFeed is a conditional GET: an unchanged feed answers 304 and the held
// bytes are verified again against the current receipt.
func (r *Reader) fetchFeed(ctx context.Context, etag string, previous []byte) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.feedURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/json")
	if etag != "" && previous != nil {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotModified:
		if previous == nil {
			return nil, "", fmt.Errorf("appreleases: 304 without a held feed")
		}
		return previous, etag, nil
	case http.StatusOK:
	default:
		return nil, "", fmt.Errorf("appreleases: feed status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFeedBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxFeedBytes {
		return nil, "", fmt.Errorf("appreleases: feed exceeds %d bytes", maxFeedBytes)
	}
	return body, response.Header.Get("ETag"), nil
}

func (r *Reader) fetchReceipt(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.receiptURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("appreleases: receipt status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReceiptBytes+1))
	if err != nil || len(body) > maxReceiptBytes {
		return "", fmt.Errorf("appreleases: receipt unreadable")
	}
	var decoded receipt
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.SchemaVersion != ReceiptSchemaVersion ||
		!strings.HasPrefix(decoded.Feed.SHA256, "sha256:") {
		return "", fmt.Errorf("appreleases: receipt invalid")
	}
	return decoded.Feed.SHA256, nil
}

func decodeFeed(body []byte) (*Feed, map[string]int, error) {
	var feed Feed
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&feed); err != nil {
		return nil, nil, err
	}
	if feed.SchemaVersion != SchemaVersion || feed.GeneratedAt.IsZero() || len(feed.Applications) == 0 {
		return nil, nil, fmt.Errorf("appreleases: not a %s feed", SchemaVersion)
	}
	index := make(map[string]int, len(feed.Applications))
	for i, application := range feed.Applications {
		if key := imageKey(application.Image); key != "" {
			if _, taken := index[key]; !taken {
				index[key] = i
			}
		}
	}
	return &feed, index, nil
}

// imageKey is the registry and repository of an image reference; tag and
// digest do not take part.
func imageKey(reference string) string {
	image := appcatalog.ParseImage(reference)
	if image.Repo == "" {
		return ""
	}
	return image.Registry + "/" + image.Repo
}

// Application returns the feed entry watching the image's repository.
func (s Status) Application(image string) (*Application, bool) {
	if !s.Verified || s.feed == nil {
		return nil, false
	}
	position, ok := s.index[imageKey(image)]
	if !ok {
		return nil, false
	}
	return &s.feed.Applications[position], true
}
