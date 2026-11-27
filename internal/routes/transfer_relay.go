package routes

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/homelab/pkg/artifactdelivery"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

const (
	transferRelayPathPrefix      = "/api/v1/internal/transfer-relay/"
	transferRelayRoutePattern    = "/api/v1/internal/transfer-relay/{transferId}/{sha256}"
	transferRelayDefaultTTL      = 30 * time.Minute
	transferRelayMaxTTL          = 2 * time.Hour
	transferRelayActiveGrace     = 30 * time.Minute
	transferRelayDefaultMax      = int64(10 << 30) // 10 GiB default per-slot ciphertext cap
	transferRelayDefaultMaxTotal = int64(20 << 30) // 20 GiB default aggregate staged ciphertext cap
	transferRelayMaxSlots        = 64
	transferRelaySweepEvery      = 1 * time.Minute
)

var (
	transferRelayIDPattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{3,127}$`)
	transferRelaySHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// TransferRelayConfig configures the bounded, single-use ciphertext relay.
type TransferRelayConfig struct {
	// BaseURL is the externally reachable Homelab origin (https://...) that
	// nodes use to reach PUT/GET /api/v1/internal/transfer-relay/{transferId}/{sha256}.
	BaseURL string
	// Dir is the private staging directory (0700) for in-flight ciphertext.
	// When empty, a dedicated private temp directory is created on first use.
	Dir string
	// MaxBytes is the maximum ciphertext size accepted per slot. Defaults to 10 GiB.
	MaxBytes int64
	// MaxTotalBytes is the aggregate ciphertext storage cap across all staged
	// slots. Defaults to HOMELAB_TRANSFER_RELAY_MAX_TOTAL_BYTES or 20 GiB.
	MaxTotalBytes int64
	// Now overrides the clock for deterministic TTL expiry checks.
	Now func() time.Time
}

type relaySlotState int

const (
	relaySlotPending relaySlotState = iota
	relaySlotUploading
	relaySlotReady
	relaySlotDownloading
	relaySlotFailed
)

type transferRelaySlotRecord struct {
	transferID        string
	archiveSHA256     string
	uploadTokenHash   [32]byte
	downloadTokenHash [32]byte
	state             relaySlotState
	filePath          string
	size              int64
	maxBytes          int64
	expiresAt         time.Time
}

// TransferRelayStore manages bounded, single-use, capability-scoped ciphertext
// relay slots for NAT-capable native application transfers. It has zero
// plaintext visibility and never logs tokens or payload bytes.
type TransferRelayStore struct {
	baseURL       string
	dir           string
	autoDir       bool
	maxBytes      int64
	maxTotalBytes int64
	now           func() time.Time

	mu               sync.Mutex
	slots            map[string]*transferRelaySlotRecord
	totalStagedBytes int64
	closed           bool
	stopCh           chan struct{}
}

// NewTransferRelayStore constructs a ciphertext relay store. Returns nil if
// BaseURL is empty or fails relay URL validation (https:// or loopback http://)
// so callers fail closed when no valid relay origin exists.
func NewTransferRelayStore(cfg TransferRelayConfig) *TransferRelayStore {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil
	}
	if err := stackkitcommand.ValidateApplicationTransferRelayURL(baseURL, true); err != nil {
		return nil
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = transferRelayDefaultMax
	}
	maxTotalBytes := cfg.MaxTotalBytes
	if maxTotalBytes <= 0 {
		if raw := strings.TrimSpace(os.Getenv("HOMELAB_TRANSFER_RELAY_MAX_TOTAL_BYTES")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
				maxTotalBytes = parsed
			}
		}
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = transferRelayDefaultMaxTotal
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	s := &TransferRelayStore{
		baseURL:       baseURL,
		dir:           strings.TrimSpace(cfg.Dir),
		maxBytes:      maxBytes,
		maxTotalBytes: maxTotalBytes,
		now:           nowFn,
		slots:         make(map[string]*transferRelaySlotRecord),
		stopCh:        make(chan struct{}),
	}
	go s.sweepLoop()
	return s
}

// Close stops background expiry sweeps and removes any staged ciphertext files
// and any auto-created temporary staging directory.
func (s *TransferRelayStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stopCh)
	var files []string
	for key, slot := range s.slots {
		if slot.filePath != "" {
			files = append(files, slot.filePath)
		}
		delete(s.slots, key)
	}
	s.totalStagedBytes = 0
	autoDir := s.autoDir
	dir := s.dir
	s.mu.Unlock()
	for _, f := range files {
		_ = os.Remove(f)
	}
	if autoDir && dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// BaseURL returns the configured relay origin.
func (s *TransferRelayStore) BaseURL() string {
	if s == nil {
		return ""
	}
	return s.baseURL
}

func relaySlotKey(transferID, shaHex string) string {
	return transferID + "\x00" + shaHex
}

func randomRelayToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func (s *TransferRelayStore) ensureDirLocked() (string, error) {
	if s.dir != "" {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(s.dir, 0o700); err != nil {
			return "", err
		}
		return s.dir, nil
	}
	dir, err := os.MkdirTemp("", "homelab-transfer-relay-*")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	s.dir = dir
	s.autoDir = true
	return dir, nil
}

// AllocateSlot mints one single-use upload+download relay slot for an exact
// transferId and ciphertext SHA-256.
func (s *TransferRelayStore) AllocateSlot(transferID, archiveSHA256 string, ttl time.Duration, maxBytes int64) (jobs.TransferRelaySlot, error) {
	if s == nil || s.baseURL == "" {
		return jobs.TransferRelaySlot{}, errors.New("transfer relay is unavailable")
	}
	transferID = strings.TrimSpace(transferID)
	shaHex := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(archiveSHA256), "sha256:"))
	if !transferRelayIDPattern.MatchString(transferID) {
		return jobs.TransferRelaySlot{}, errors.New("transfer relay id is invalid")
	}
	if !transferRelaySHA256Pattern.MatchString(shaHex) {
		return jobs.TransferRelaySlot{}, errors.New("transfer relay archive sha256 is invalid")
	}
	if ttl <= 0 {
		ttl = transferRelayDefaultTTL
	}
	if ttl > transferRelayMaxTTL {
		ttl = transferRelayMaxTTL
	}
	capBytes := s.maxBytes
	if maxBytes > 0 && maxBytes < capBytes {
		capBytes = maxBytes
	}
	uploadToken, uploadHash, err := randomRelayToken()
	if err != nil {
		return jobs.TransferRelaySlot{}, err
	}
	downloadToken, downloadHash, err := randomRelayToken()
	if err != nil {
		return jobs.TransferRelaySlot{}, err
	}
	now := s.now()
	expiresAt := now.Add(ttl)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return jobs.TransferRelaySlot{}, errors.New("transfer relay is closed")
	}
	expiredFiles := s.evictExpiredLocked(now)
	if len(s.slots) >= transferRelayMaxSlots {
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		return jobs.TransferRelaySlot{}, errors.New("transfer relay capacity limit reached")
	}
	key := relaySlotKey(transferID, shaHex)
	if existing, exists := s.slots[key]; exists {
		if existing.state == relaySlotUploading || existing.state == relaySlotDownloading {
			s.mu.Unlock()
			removeFilesBestEffort(expiredFiles)
			return jobs.TransferRelaySlot{}, errors.New("transfer relay slot is currently active")
		}
		s.subStagedBytesLocked(existing.size)
		if existing.filePath != "" {
			expiredFiles = append(expiredFiles, existing.filePath)
		}
		delete(s.slots, key)
	}
	s.slots[key] = &transferRelaySlotRecord{
		transferID:        transferID,
		archiveSHA256:     shaHex,
		uploadTokenHash:   uploadHash,
		downloadTokenHash: downloadHash,
		state:             relaySlotPending,
		maxBytes:          capBytes,
		expiresAt:         expiresAt,
	}
	s.mu.Unlock()
	removeFilesBestEffort(expiredFiles)

	return jobs.TransferRelaySlot{
		TransferID:    transferID,
		ArchiveSHA256: "sha256:" + shaHex,
		RelayURL:      s.baseURL + transferRelayPathPrefix + transferID + "/" + shaHex,
		UploadToken:   uploadToken,
		DownloadToken: downloadToken,
		ExpiresAt:     expiresAt,
	}, nil
}

func (s *TransferRelayStore) subStagedBytesLocked(n int64) {
	if n <= 0 {
		return
	}
	s.totalStagedBytes -= n
	if s.totalStagedBytes < 0 {
		s.totalStagedBytes = 0
	}
}

// ReleaseSlot deletes any staged ciphertext file and removes the relay slot.
func (s *TransferRelayStore) ReleaseSlot(transferID, archiveSHA256 string) {
	if s == nil {
		return
	}
	transferID = strings.TrimSpace(transferID)
	shaHex := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(archiveSHA256), "sha256:"))
	key := relaySlotKey(transferID, shaHex)
	s.mu.Lock()
	slot, ok := s.slots[key]
	var filePath string
	if ok {
		s.subStagedBytesLocked(slot.size)
		filePath = slot.filePath
		delete(s.slots, key)
	}
	s.mu.Unlock()
	if filePath != "" {
		_ = os.Remove(filePath)
	}
}

// markSlotFailed transitions an active upload slot to relaySlotFailed so any
// waiting downloader receives 410 Gone immediately instead of retrying until timeout.
func (s *TransferRelayStore) markSlotFailed(key string, expected *transferRelaySlotRecord) {
	if s == nil {
		return
	}
	s.mu.Lock()
	var filePath string
	if current, ok := s.slots[key]; ok && current == expected {
		s.subStagedBytesLocked(current.size)
		filePath = current.filePath
		current.filePath = ""
		current.size = 0
		current.state = relaySlotFailed
	}
	s.mu.Unlock()
	if filePath != "" {
		_ = os.Remove(filePath)
	}
}

func (s *TransferRelayStore) evictExpiredLocked(now time.Time) []string {
	var files []string
	for key, slot := range s.slots {
		if !now.After(slot.expiresAt) {
			continue
		}
		if (slot.state == relaySlotUploading || slot.state == relaySlotDownloading) &&
			!now.After(slot.expiresAt.Add(transferRelayActiveGrace)) {
			continue
		}
		s.subStagedBytesLocked(slot.size)
		if slot.filePath != "" {
			files = append(files, slot.filePath)
		}
		delete(s.slots, key)
	}
	return files
}

func (s *TransferRelayStore) sweepLoop() {
	ticker := time.NewTicker(transferRelaySweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.mu.Lock()
			files := s.evictExpiredLocked(s.now())
			s.mu.Unlock()
			removeFilesBestEffort(files)
		}
	}
}

func removeFilesBestEffort(paths []string) {
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

func extractBearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) < 8 || !strings.EqualFold(auth[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

func tokenMatchesHash(token string, expected [32]byte) bool {
	if token == "" {
		return false
	}
	actual := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(actual[:], expected[:]) == 1
}

// RegisterTransferRelayRoutes wires the single-use encrypted transfer relay
// endpoints onto the router.
func RegisterTransferRelayRoutes(r *httpx.Router, relay *TransferRelayStore) {
	if r == nil || relay == nil {
		return
	}
	r.PUT(transferRelayRoutePattern, relay.handleUpload)
	r.GET(transferRelayRoutePattern, relay.handleDownload)
}

func (s *TransferRelayStore) handleUpload(e *httpx.Event) error {
	transferID := strings.TrimSpace(e.Request.PathValue("transferId"))
	shaHex := strings.ToLower(strings.TrimSpace(e.Request.PathValue("sha256")))
	if !transferRelayIDPattern.MatchString(transferID) || !transferRelaySHA256Pattern.MatchString(shaHex) {
		http.Error(e.Response, "invalid transfer relay target", http.StatusBadRequest)
		return nil
	}
	if headerDigest := strings.TrimSpace(e.Request.Header.Get("X-Transfer-Archive-SHA256")); headerDigest != "" {
		normalized := strings.ToLower(strings.TrimPrefix(headerDigest, "sha256:"))
		if normalized != shaHex {
			http.Error(e.Response, "archive digest header mismatch", http.StatusBadRequest)
			return nil
		}
	}
	token := extractBearerToken(e.Request)
	if token == "" {
		http.Error(e.Response, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	key := relaySlotKey(transferID, shaHex)
	now := s.now()

	s.mu.Lock()
	expiredFiles := s.evictExpiredLocked(now)
	slot, ok := s.slots[key]
	if !ok {
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "forbidden", http.StatusForbidden)
		return nil
	}
	if !tokenMatchesHash(token, slot.uploadTokenHash) {
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "forbidden", http.StatusForbidden)
		return nil
	}
	if slot.state != relaySlotPending {
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "upload slot already used", http.StatusConflict)
		return nil
	}
	maxBytes := slot.maxBytes
	if e.Request.ContentLength > maxBytes {
		slot.state = relaySlotFailed
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "ciphertext exceeds relay size cap", http.StatusRequestEntityTooLarge)
		return nil
	}
	if e.Request.ContentLength > 0 && s.totalStagedBytes+e.Request.ContentLength > s.maxTotalBytes {
		slot.state = relaySlotFailed
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "transfer relay aggregate storage limit reached", http.StatusInsufficientStorage)
		return nil
	}
	dir, dirErr := s.ensureDirLocked()
	if dirErr != nil {
		slot.state = relaySlotFailed
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "relay storage unavailable", http.StatusServiceUnavailable)
		return nil
	}
	slot.state = relaySlotUploading
	s.mu.Unlock()
	removeFilesBestEffort(expiredFiles)

	var randName [16]byte
	if _, err := rand.Read(randName[:]); err != nil {
		s.markSlotFailed(key, slot)
		http.Error(e.Response, "relay storage unavailable", http.StatusInternalServerError)
		return nil
	}
	filePath := filepath.Join(dir, "slot-"+hex.EncodeToString(randName[:])+".age")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.markSlotFailed(key, slot)
		http.Error(e.Response, "relay storage unavailable", http.StatusServiceUnavailable)
		return nil
	}
	keepFile := false
	defer func() {
		_ = file.Close()
		if !keepFile {
			_ = os.Remove(filePath)
		}
	}()

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(e.Request.Body, maxBytes+1))
	if copyErr != nil {
		s.markSlotFailed(key, slot)
		http.Error(e.Response, "failed reading ciphertext stream", http.StatusBadRequest)
		return nil
	}
	if written > maxBytes {
		s.markSlotFailed(key, slot)
		http.Error(e.Response, "ciphertext exceeds relay size cap", http.StatusRequestEntityTooLarge)
		return nil
	}
	if hex.EncodeToString(hash.Sum(nil)) != shaHex {
		s.markSlotFailed(key, slot)
		http.Error(e.Response, "ciphertext digest mismatch", http.StatusUnprocessableEntity)
		return nil
	}
	if err := file.Close(); err != nil {
		s.markSlotFailed(key, slot)
		http.Error(e.Response, "relay storage write failed", http.StatusInternalServerError)
		return nil
	}

	s.mu.Lock()
	current, stillPresent := s.slots[key]
	if !stillPresent || current != slot || s.now().After(current.expiresAt) {
		if stillPresent && current == slot {
			s.subStagedBytesLocked(current.size)
			delete(s.slots, key)
		}
		s.mu.Unlock()
		http.Error(e.Response, "relay slot expired", http.StatusGone)
		return nil
	}
	if s.totalStagedBytes+written > s.maxTotalBytes {
		current.state = relaySlotFailed
		s.mu.Unlock()
		http.Error(e.Response, "transfer relay aggregate storage limit reached", http.StatusInsufficientStorage)
		return nil
	}
	current.filePath = filePath
	current.size = written
	current.state = relaySlotReady
	s.totalStagedBytes += written
	keepFile = true
	s.mu.Unlock()

	e.Response.Header().Set("Content-Type", "application/json")
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(e.Response, `{"status":"staged","bytes":%d,"sha256":"%s"}`+"\n", written, shaHex)
	return nil
}

func (s *TransferRelayStore) handleDownload(e *httpx.Event) error {
	// Render serves no download (workspace CI-CD "Render download prohibition"):
	// refuse before any slot or file is touched.
	if err := artifactdelivery.RequireAllowed(e); err != nil {
		return err
	}
	transferID := strings.TrimSpace(e.Request.PathValue("transferId"))
	shaHex := strings.ToLower(strings.TrimSpace(e.Request.PathValue("sha256")))
	if !transferRelayIDPattern.MatchString(transferID) || !transferRelaySHA256Pattern.MatchString(shaHex) {
		http.Error(e.Response, "invalid transfer relay target", http.StatusBadRequest)
		return nil
	}
	token := extractBearerToken(e.Request)
	if token == "" {
		http.Error(e.Response, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	key := relaySlotKey(transferID, shaHex)
	now := s.now()

	s.mu.Lock()
	expiredFiles := s.evictExpiredLocked(now)
	slot, ok := s.slots[key]
	if !ok {
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "not found", http.StatusNotFound)
		return nil
	}
	if !tokenMatchesHash(token, slot.downloadTokenHash) {
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "forbidden", http.StatusForbidden)
		return nil
	}
	switch slot.state {
	case relaySlotPending, relaySlotUploading:
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		e.Response.Header().Set("Retry-After", "2")
		http.Error(e.Response, "ciphertext upload in progress", http.StatusServiceUnavailable)
		return nil
	case relaySlotDownloading:
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "download slot already in use", http.StatusConflict)
		return nil
	case relaySlotFailed:
		s.mu.Unlock()
		removeFilesBestEffort(expiredFiles)
		http.Error(e.Response, "ciphertext upload failed", http.StatusGone)
		return nil
	case relaySlotReady:
		slot.state = relaySlotDownloading
	}
	filePath := slot.filePath
	size := slot.size
	s.mu.Unlock()
	removeFilesBestEffort(expiredFiles)

	// Single-use download: always remove the slot and delete staged ciphertext
	// immediately once the download finishes (or fails).
	defer func() {
		s.mu.Lock()
		if current, exists := s.slots[key]; exists && current == slot {
			s.subStagedBytesLocked(current.size)
			delete(s.slots, key)
		}
		s.mu.Unlock()
		if filePath != "" {
			_ = os.Remove(filePath)
		}
	}()

	file, err := os.Open(filePath)
	if err != nil {
		http.Error(e.Response, "staged ciphertext unavailable", http.StatusGone)
		return nil
	}
	defer file.Close()

	e.Response.Header().Set("Content-Type", "application/octet-stream")
	e.Response.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	e.Response.Header().Set("X-Transfer-Archive-SHA256", shaHex)
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.WriteHeader(http.StatusOK)
	_, _ = io.Copy(e.Response, file)
	return nil
}
