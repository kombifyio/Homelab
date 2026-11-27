package diskcustody

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kombifyio/homelab/pkg/unifier"
)

const (
	// legacyListLimit caps the names one report line carries.
	legacyListLimit = 50
	// archiveVersionLimit caps the stored versions of one archived path, so a
	// file that keeps changing cannot grow the archive on every boot.
	archiveVersionLimit = 3
	// maxArchiveFileBytes bounds one archived file read into memory so an
	// oversized file on the data disk cannot OOM-loop at boot.
	maxArchiveFileBytes int64 = 64 << 20
)

var errArchiveFileTooLarge = errors.New("archive file exceeds size limit")

// knownTopLevel are the entries of the data directory the import accounts for:
// handled above, history that is rebuilt (the runtime log spool), self-hosted
// only state, and the legacy roots it inventories. Any other entry is reported
// as unknown and keeps the import incomplete.
var knownTopLevel = map[string]bool{
	stacksDir: true, enrollmentFile: true, instanceFile: true, hostKeyFile: true, knownHostsFile: true,
	"runtime-logs.jsonl": true,
	"postgres":           true, "lan-bridge": true, "lan-bridge-enabled": true,
	"pb_data": true, "state": true, "coolify": true, "stackkits": true, "backups": true,
}

// legacyRoots are the entries the import copies into the tenant-free archive.
// The state directory (pb_data on an existing installation, state on a new
// one) is where current code writes only the rebuilt children in
// legacySkippedDirs; coolify, stackkits and backups no current code reads or
// writes. An older installation's disk may hold a retired PocketBase database
// or credentials in any of them, so every file is copied byte for byte before
// the disk goes and the owner decides afterwards what to do with it.
var legacyRoots = []string{"pb_data", "state", "coolify", "stackkits", "backups"}

// legacySkippedDirs are the state directory's rebuilt children: the monitoring history and the
// agent CA and its server key, which never leave the disk (agent gRPC is off
// in production).
var legacySkippedDirs = map[string]bool{
	"monitoring": true, "agent-ca": true, "agent-grpc-tls": true, "agent-grpc-cert": true,
}

// accountForRemainder copies what the rest of the import does not cover into
// the tenant-free archive.
//
// Unowned stacks (unresolved: no tenant confirmed, or orphaned: the tenant the
// intent names holds no such row, which cannot be told from a stack of a
// tenant the directory misses): every file is copied into the tenant-free
// archive, rows only added (an operator can remove them again). A stack of a
// tenant is restored by the runtime only from its tenant-scoped custody, so
// the archive is the copy that outlives the disk.
//
// Legacy directories are copied the same way: a retired database or a
// credential on the disk survives it. Unknown top-level entries are listed
// with their size and keep the import incomplete: the import does not know
// what they hold.
func accountForRemainder(ctx context.Context, pool *sql.DB, dataDir string, report *ImportReport) error {
	var errs []error
	unowned := append(append([]string{}, report.StacksUnresolved...), report.StacksOrphaned...)
	for _, id := range unowned {
		dir := filepath.Join(dataDir, stacksDir, id)
		carries := false
		walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == ".terraform" {
					return filepath.SkipDir // provider plugins OpenTofu downloads again
				}
				return nil
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			carries = true
			report.UnownedFiles++
			report.UnownedBytes += info.Size()
			if !entry.Type().IsRegular() {
				return nil
			}
			// Every file of the stack is copied, not only the managed spec
			// files: a nested file (the tofu working directory) is the only
			// copy that exists once the disk is gone.
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				return relErr
			}
			data, readErr := readBoundedArchiveFile(path, maxArchiveFileBytes)
			if errors.Is(readErr, errArchiveFileTooLarge) {
				errs = append(errs, fmt.Errorf("unowned stack %s: %w", id, readErr))
				return nil
			}
			if readErr != nil {
				return readErr
			}
			source := stacksDir + "/" + id + "/" + filepath.ToSlash(rel)
			matched, archiveErr := archiveVersioned(ctx, pool, source, data)
			if archiveErr != nil {
				return archiveErr
			}
			if matched {
				report.UnownedMatching++
			}
			// A managed spec file lives at the top of the stack directory; a
			// nested file of the same name is working state, not custody.
			if filepath.Dir(path) != dir || !unifier.IsManagedSpecFilename(entry.Name()) {
				return nil
			}
			report.UnownedSpecFiles++
			if matched {
				report.UnownedSpecMatching++
			}
			return nil
		})
		if walkErr != nil {
			errs = append(errs, fmt.Errorf("unowned stack %s: %w", id, walkErr))
		}
		if carries {
			report.StacksUnownedWithFiles = append(report.StacksUnownedWithFiles, id)
		}
	}
	sort.Strings(report.StacksUnownedWithFiles)

	top, err := os.ReadDir(dataDir)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, entry := range top {
		// A symlink is never followed, so whatever it names is not accounted for.
		if !knownTopLevel[entry.Name()] || entry.Type()&os.ModeSymlink != 0 {
			report.UnknownEntries = append(report.UnknownEntries, entry.Name())
		}
	}
	sort.Strings(report.UnknownEntries)

	var listed []string
	for _, name := range legacyRoots {
		root := filepath.Join(dataDir, name)
		if info, err := os.Lstat(root); err != nil || !info.IsDir() {
			continue
		}
		stateDir := name == "pb_data" || name == "state"
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				// Only the state directory's own rebuilt children are skipped.
				if stateDir && filepath.Dir(path) == root && legacySkippedDirs[entry.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			rel, _ := filepath.Rel(dataDir, path)
			report.LegacyFiles++
			report.LegacyBytes += info.Size()
			listed = append(listed, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), info.Size()))
			// The legacy directory can hold a retired database or a credential.
			// It is copied byte for byte so the disk's content survives the
			// disk, and the owner decides afterwards what to do with it.
			data, readErr := readBoundedArchiveFile(path, maxArchiveFileBytes)
			if errors.Is(readErr, errArchiveFileTooLarge) {
				errs = append(errs, fmt.Errorf("%s: %w", name, readErr))
				return nil
			}
			if readErr != nil {
				return readErr
			}
			matched, archiveErr := archiveVersioned(ctx, pool, filepath.ToSlash(rel), data)
			if archiveErr != nil {
				return archiveErr
			}
			if matched {
				report.LegacyMatching++
			}
			return nil
		})
		if walkErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, walkErr))
		}
	}
	sort.Strings(listed)
	report.LegacyListing = strings.Join(capNames(listed), " ")
	return errors.Join(errs...)
}

// readBoundedArchiveFile reads at most maxBytes from path, failing before
// allocating when os.Stat reports a larger file and capping the read stream
// with io.LimitReader in case the file grows concurrently.
func readBoundedArchiveFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %s (%d bytes > %d byte limit)", errArchiveFileTooLarge, path, info.Size(), maxBytes)
	}
	file, err := os.Open(path) // #nosec G304 -- walked file under the data directory.
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s (> %d byte limit)", errArchiveFileTooLarge, path, maxBytes)
	}
	return data, nil
}

// archiveVersioned stores one file in the tenant-free archive (rows only
// added) and verifies the stored bytes via the bounded SECURITY DEFINER
// digest-only readback routine (migration 207), never selecting content bytes.
// A path that already holds archiveVersionLimit other versions is left alone
// and reported as not matching.
func archiveVersioned(ctx context.Context, pool *sql.DB, source string, data []byte) (bool, error) {
	digest := sha256Hex(data)
	status, err := readControlPlaneArchiveDigest(ctx, pool, source, digest)
	if errors.Is(err, errControlPlaneArchiveDigestPending) {
		return archiveVersionedLegacy(ctx, pool, source, digest, data)
	}
	if err != nil {
		return false, err
	}
	if status.HasMatch && status.ContentSHA256 == digest && status.ByteSize == int64(len(data)) {
		return true, nil
	}
	if status.OtherVersions >= archiveVersionLimit {
		return false, nil
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO control_plane_disk_archive (source_path, sha256, byte_size, content)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, source, digest, len(data), data); err != nil {
		return false, err
	}
	status, err = readControlPlaneArchiveDigest(ctx, pool, source, digest)
	if err != nil {
		return false, err
	}
	return status.HasMatch && status.ContentSHA256 == digest && status.ByteSize == int64(len(data)), nil
}

// archiveVersionedLegacy preserves pre-207 verification when the digest
// routine is not rolled out yet (42883 / 42P01).
func archiveVersionedLegacy(ctx context.Context, pool *sql.DB, source, digest string, data []byte) (bool, error) {
	var others int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM control_plane_disk_archive WHERE source_path = $1 AND sha256 <> $2`,
		source, digest).Scan(&others); err != nil {
		return false, err
	}
	if others >= archiveVersionLimit {
		return false, nil
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO control_plane_disk_archive (source_path, sha256, byte_size, content)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, source, digest, len(data), data); err != nil {
		return false, err
	}
	var held string
	if err := pool.QueryRowContext(ctx, `SELECT encode(sha256(content), 'hex') FROM control_plane_disk_archive
		WHERE source_path = $1 AND sha256 = $2`, source, digest).Scan(&held); err != nil {
		return false, err
	}
	return held == digest, nil
}
