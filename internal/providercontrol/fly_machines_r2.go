package providercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const (
	flyR2MaxPayloadBytes  = 4 << 20
	flyR2MaxEnvelopeBytes = 6 << 20
)

var (
	flyR2AccountPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	flyR2BucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// FlyWorkloadPayloadBridge moves real workload data bytes out of and into a
// Fly Machine during backup and restore. Without a wired data-plane bridge,
// FlyMachinesR2Store fails closed on Backup and Restore so metadata-only writes
// never masquerade as workload backups.
type FlyWorkloadPayloadBridge interface {
	Export(ctx context.Context, handle FlyMachineHandle) ([]byte, error)
	Import(ctx context.Context, handle FlyMachineHandle, payload []byte) error
}

type FlyMachinesR2Config struct {
	AccountID       string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PayloadBridge   FlyWorkloadPayloadBridge
}

type FlyMachinesR2Store struct {
	bucket string
	client *s3.Client
	bridge FlyWorkloadPayloadBridge
}

type flyR2Snapshot struct {
	App           string `json:"app"`
	Region        string `json:"region"`
	MachineID     string `json:"machine_id"`
	TenantID      string `json:"tenant_id"`
	WorkloadID    string `json:"workload_id"`
	SlotRef       string `json:"slot_ref"`
	CustodyHash   string `json:"custody_hash"`
	PayloadSHA256 string `json:"payload_sha256"`
	PayloadBase64 string `json:"payload_base64"`
	CapturedAt    string `json:"captured_at"`
}

// NewFlyMachinesR2Store creates a tenant-confined EU R2 custody store pinned to
// Cloudflare's EU R2 endpoint (<account_id>.eu.r2.cloudflarestorage.com).
func NewFlyMachinesR2Store(cfg FlyMachinesR2Config) (*FlyMachinesR2Store, error) {
	return newFlyMachinesR2Store(cfg, "")
}

func newFlyMachinesR2Store(cfg FlyMachinesR2Config, endpointOverride string) (*FlyMachinesR2Store, error) {
	if !flyR2AccountPattern.MatchString(cfg.AccountID) || !flyR2BucketPattern.MatchString(cfg.Bucket) ||
		strings.TrimSpace(cfg.AccessKeyID) == "" || strings.TrimSpace(cfg.SecretAccessKey) == "" {
		return nil, &FlyMachinesReadinessError{Code: "fly_r2_configuration_invalid"}
	}
	endpoint := "https://" + cfg.AccountID + ".eu.r2.cloudflarestorage.com"
	if endpointOverride != "" {
		endpoint = endpointOverride
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := s3.New(s3.Options{
		BaseEndpoint:               aws.String(endpoint),
		Region:                     "auto",
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	})
	return &FlyMachinesR2Store{bucket: cfg.Bucket, client: client, bridge: cfg.PayloadBridge}, nil
}

// BackupReady returns true only when a workload data-plane payload bridge is
// wired. Without it, Backup and Restore refuse to claim success.
func (store *FlyMachinesR2Store) BackupReady() bool {
	return store != nil && store.bridge != nil
}

func (store *FlyMachinesR2Store) Backup(ctx context.Context, handle FlyMachineHandle) (FlyBackupArtifact, error) {
	if store == nil || store.bridge == nil || !flyMachinePathID(handle.App) || !flyMachinePathID(handle.MachineID) ||
		handle.Owner.TenantID == "" || !flyIdentifierValid(handle.Owner.WorkloadID) || handle.Owner.SlotRef == "" || handle.Owner.CustodyHash == "" {
		return FlyBackupArtifact{}, ErrFlyMachineBackup
	}
	payload, err := store.bridge.Export(ctx, handle)
	if err != nil || len(payload) == 0 || len(payload) > flyR2MaxPayloadBytes {
		return FlyBackupArtifact{}, ErrFlyMachineBackup
	}
	payloadSum := sha256.Sum256(payload)
	snapshot := flyR2Snapshot{
		App:           handle.App,
		Region:        FlyMachinesRegion,
		MachineID:     handle.MachineID,
		TenantID:      handle.Owner.TenantID,
		WorkloadID:    handle.Owner.WorkloadID,
		SlotRef:       handle.Owner.SlotRef,
		CustodyHash:   handle.Owner.CustodyHash,
		PayloadSHA256: hex.EncodeToString(payloadSum[:]),
		PayloadBase64: base64.StdEncoding.EncodeToString(payload),
		CapturedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) == 0 || len(data) > flyR2MaxEnvelopeBytes {
		return FlyBackupArtifact{}, ErrFlyMachineBackup
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	key := flyBackupKeyPrefix(handle.Owner.TenantID, handle.Owner.WorkloadID) + digest + ".json"
	_, err = store.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(store.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentType:   aws.String("application/json"),
		IfNoneMatch:   aws.String("*"),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		var serviceErr smithy.APIError
		if !errors.As(err, &serviceErr) || serviceErr.ErrorCode() != "PreconditionFailed" {
			return FlyBackupArtifact{}, ErrFlyMachineBackup
		}
	}
	artifact := FlyBackupArtifact{Ref: key, SHA256: digest}
	if _, err := store.verify(ctx, handle, artifact); err != nil {
		return FlyBackupArtifact{}, ErrFlyMachineBackup
	}
	return artifact, nil
}

func (store *FlyMachinesR2Store) Restore(ctx context.Context, handle FlyMachineHandle, artifact FlyBackupArtifact) error {
	if store == nil || store.bridge == nil {
		return ErrFlyMachineBackup
	}
	payload, err := store.verify(ctx, handle, artifact)
	if err != nil {
		return err
	}
	if err := store.bridge.Import(ctx, handle, payload); err != nil {
		return ErrFlyMachineBackup
	}
	return nil
}

// PurgeWorkload deletes all R2 backup artifacts strictly under the owning
// tenant and workload prefix before a decommissioned generation releases its slot.
func (store *FlyMachinesR2Store) PurgeWorkload(ctx context.Context, tenantID, workloadID string) error {
	if store == nil || strings.TrimSpace(tenantID) == "" || !flyIdentifierValid(workloadID) {
		return ErrFlyMachineBackup
	}
	prefix := flyBackupKeyPrefix(tenantID, workloadID)
	var continuationToken *string
	for {
		page, err := store.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(store.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return ErrFlyMachineBackup
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if key == "" || !strings.HasPrefix(key, prefix) {
				return ErrFlyMachineBackup
			}
			if _, err := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(store.bucket),
				Key:    aws.String(key),
			}); err != nil {
				return ErrFlyMachineBackup
			}
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			break
		}
		continuationToken = page.NextContinuationToken
	}
	return nil
}

func (store *FlyMachinesR2Store) verify(ctx context.Context, handle FlyMachineHandle, artifact FlyBackupArtifact) ([]byte, error) {
	if !artifact.valid() || !flyIdentifierValid(handle.Owner.WorkloadID) {
		return nil, ErrFlyMachineBackup
	}
	expectedKey := flyBackupKeyPrefix(handle.Owner.TenantID, handle.Owner.WorkloadID) + artifact.SHA256 + ".json"
	if artifact.Ref != expectedKey {
		return nil, ErrFlyMachineBackup
	}
	object, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(artifact.Ref),
	})
	if err != nil {
		return nil, ErrFlyMachineBackup
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, flyR2MaxEnvelopeBytes+1))
	if err != nil || len(data) == 0 || len(data) > flyR2MaxEnvelopeBytes {
		return nil, ErrFlyMachineBackup
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return nil, ErrFlyMachineBackup
	}
	var snapshot flyR2Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, ErrFlyMachineBackup
	}
	if snapshot.App != handle.App || snapshot.Region != FlyMachinesRegion ||
		snapshot.TenantID != handle.Owner.TenantID || snapshot.WorkloadID != handle.Owner.WorkloadID ||
		snapshot.SlotRef != handle.Owner.SlotRef || snapshot.CustodyHash != handle.Owner.CustodyHash {
		return nil, ErrFlyMachineBackup
	}
	payload, err := base64.StdEncoding.DecodeString(snapshot.PayloadBase64)
	if err != nil || len(payload) == 0 || len(payload) > flyR2MaxPayloadBytes {
		return nil, ErrFlyMachineBackup
	}
	payloadSum := sha256.Sum256(payload)
	if hex.EncodeToString(payloadSum[:]) != snapshot.PayloadSHA256 {
		return nil, ErrFlyMachineBackup
	}
	return payload, nil
}

func flyBackupKeyPrefix(tenantID, workloadID string) string {
	tenantSum := sha256.Sum256([]byte(tenantID))
	return "tenants/" + hex.EncodeToString(tenantSum[:]) + "/workloads/" + workloadID + "/backups/"
}
