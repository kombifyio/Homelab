package ionosworker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"golang.org/x/sys/unix"
)

const maxBackupBytes = 512 * 1024 * 1024

var (
	r2Account = regexp.MustCompile(`^[0-9a-f]{32}$`)
	r2Bucket  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	r2Digest  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type R2Credential struct {
	AccountID       string `json:"account_id"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

type BackupStore struct {
	bucket string
	client *s3.Client
}

func NewBackupStore(credential R2Credential) (*BackupStore, error) {
	if !r2Account.MatchString(credential.AccountID) || !r2Bucket.MatchString(credential.Bucket) || credential.AccessKeyID == "" || credential.SecretAccessKey == "" {
		return nil, errors.New("invalid worker R2 credential")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String("https://" + credential.AccountID + ".eu.r2.cloudflarestorage.com"),
		Region:       "auto", UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider(credential.AccessKeyID, credential.SecretAccessKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	return &BackupStore{bucket: credential.Bucket, client: client}, nil
}

func (store *BackupStore) save(ctx context.Context, spec providercontrol.IONOSSharedPoolSpec, volume string) (*providercontrol.IONOSSharedPoolBackupManifest, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	scratch, err := prepareBackupScratch(volume)
	if err != nil {
		return nil, err
	}
	defer clearBackupScratch(volume)
	file, err := os.CreateTemp(scratch, ".backup-")
	if err != nil {
		return nil, err
	}
	defer removeTemporary(file)
	if err := archiveVolume(ctx, volume, file); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil || size > maxBackupBytes {
		return nil, errors.New("worker backup exceeds archive limit or is unreadable")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	manifest := &providercontrol.IONOSSharedPoolBackupManifest{
		Store: "r2", Bucket: store.bucket, Key: spec.VolumePrefix + "backups/" + digest + ".tar",
		Version: digest, SHA256: digest, SizeBytes: size, Spec: spec,
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	_, err = store.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(manifest.Key), Body: file,
		IfNoneMatch: aws.String("*"), ContentLength: aws.Int64(size),
	})
	if err != nil {
		var serviceError smithy.APIError
		if !errors.As(err, &serviceError) || serviceError.ErrorCode() != "PreconditionFailed" {
			return nil, errors.New("worker R2 write failed")
		}
	}
	if err := store.VerifyIONOSSharedPoolBackup(ctx, *manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func (store *BackupStore) VerifyIONOSSharedPoolBackup(ctx context.Context, manifest providercontrol.IONOSSharedPoolBackupManifest) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	return store.read(ctx, manifest, io.Discard)
}

func (store *BackupStore) read(ctx context.Context, manifest providercontrol.IONOSSharedPoolBackupManifest, destination io.Writer) error {
	if manifest.Store != "r2" || manifest.Bucket != store.bucket || !r2Digest.MatchString(manifest.SHA256) || manifest.Version != manifest.SHA256 || manifest.Key != manifest.Spec.VolumePrefix+"backups/"+manifest.Version+".tar" || manifest.SizeBytes <= 0 || manifest.SizeBytes > maxBackupBytes {
		return errors.New("worker R2 manifest refused")
	}
	object, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(manifest.Key)})
	if err != nil {
		return errors.New("worker R2 read failed")
	}
	defer object.Body.Close()
	if aws.ToInt64(object.ContentLength) != manifest.SizeBytes {
		return errors.New("worker R2 object size mismatch")
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(object.Body, manifest.SizeBytes+1))
	if err != nil || size != manifest.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != manifest.SHA256 {
		return errors.New("worker R2 object digest mismatch")
	}
	return nil
}

func (store *BackupStore) restore(ctx context.Context, spec providercontrol.IONOSSharedPoolSpec, volume string, manifest *providercontrol.IONOSSharedPoolBackupManifest) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	if manifest == nil || manifest.Spec != spec {
		return errors.New("worker R2 generation mismatch")
	}
	scratch, err := prepareBackupScratch(volume)
	if err != nil {
		return err
	}
	defer clearBackupScratch(volume)
	file, err := os.CreateTemp(scratch, ".restore-")
	if err != nil {
		return err
	}
	defer removeTemporary(file)
	if err := store.read(ctx, *manifest, file); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(scratch, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractVolume(ctx, staging, file); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, volume, unix.RENAME_EXCHANGE)
}

func backupScratch(volume string) string {
	return filepath.Join(filepath.Dir(volume), ".backup-"+filepath.Base(volume))
}

func clearBackupScratch(volume string) error { return os.RemoveAll(backupScratch(volume)) }

func prepareBackupScratch(volume string) (string, error) {
	if err := clearBackupScratch(volume); err != nil {
		return "", err
	}
	scratch := backupScratch(volume)
	return scratch, os.Mkdir(scratch, 0700)
}

func removeTemporary(file *os.File) {
	_ = file.Close()
	_ = os.Remove(file.Name())
}

func archiveVolume(ctx context.Context, volume string, destination io.Writer) error {
	root, err := os.OpenRoot(volume)
	if err != nil {
		return err
	}
	defer root.Close()
	written := int64(0)
	writer := tar.NewWriter(destination)
	if err := fs.WalkDir(root.FS(), ".", func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil || (!info.Mode().IsRegular() && !info.IsDir()) {
			return errors.New("worker volume contains non-file or non-directory data")
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid > 65535 || stat.Gid > 65535 {
			return errors.New("worker volume has unsupported ownership")
		}
		header.Name, header.Uid, header.Gid = filename, int(stat.Uid), int(stat.Gid)
		header.Uname, header.Gname, header.Mode = "", "", header.Mode&0777
		written += header.Size + 4096
		if written > maxBackupBytes {
			return errors.New("worker backup exceeds archive limit")
		}
		if err := writer.WriteHeader(header); err != nil || info.IsDir() {
			return err
		}
		file, err := root.Open(filename)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(writer, file, header.Size)
		return errors.Join(copyErr, file.Close())
	}); err != nil {
		return err
	}
	return writer.Close()
}

func extractVolume(ctx context.Context, destination string, source io.Reader) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(source)
	var directories []tar.Header
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !fs.ValidPath(header.Name) || path.Clean(header.Name) != header.Name || header.Uid < 0 || header.Uid > 65535 || header.Gid < 0 || header.Gid > 65535 || header.Mode < 0 || header.Mode > 0777 {
			return errors.New("worker restore archive entry refused")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Name != "." {
				if err := root.Mkdir(header.Name, 0700); err != nil {
					return err
				}
			} else if len(directories) != 0 {
				return errors.New("worker restore archive duplicates its volume root")
			}
			directories = append(directories, *header)
		case tar.TypeReg:
			file, err := root.OpenFile(header.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			if err := errors.Join(copyErr, file.Chown(header.Uid, header.Gid), file.Chmod(fs.FileMode(header.Mode)), file.Sync(), file.Close()); err != nil {
				return err
			}
		default:
			return errors.New("worker restore refuses links and special files")
		}
	}
	if len(directories) == 0 || directories[0].Name != "." {
		return errors.New("worker restore archive lacks a volume root")
	}
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		if err := root.Chown(directory.Name, directory.Uid, directory.Gid); err != nil {
			return err
		}
		if err := root.Chmod(directory.Name, fs.FileMode(directory.Mode)); err != nil {
			return err
		}
	}
	return nil
}
