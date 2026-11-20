package diskcustody

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
)

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

// legacyRoots are the entries the import inventories, never copies. The state
// directory (pb_data on an existing installation, state on a new one) is
// where current code writes only the rebuilt children in legacySkippedDirs;
// coolify, stackkits and backups no current code reads or writes. An older
// installation's disk may hold a retired PocketBase database or credentials in
// any of them, so what goes into the database is the owner's decision.
var legacyRoots = []string{"pb_data", "state", "coolify", "stackkits", "backups"}

// legacySkippedDirs are the state directory's rebuilt children: the monitoring history and the
// agent CA and its server key, which never leave the disk (agent gRPC is off
// in production).
var legacySkippedDirs = map[string]bool{
	"monitoring": true, "agent-ca": true, "agent-grpc-tls": true, "agent-grpc-cert": true,
}

// accountForRemainder records what the rest of the import does not cover.
//
// Unowned stacks (unresolved: no tenant confirmed, or orphaned: the tenant the
// intent names holds no such row, which cannot be told from a stack of a
// tenant the directory misses): their managed spec files are copied into the
// tenant-free archive, rows only added (an operator can remove them again),
// and any file other than provider plugins keeps the import incomplete,
// because only a tenant-scoped row is restored by the runtime.
//
// Legacy directories and unknown top-level entries are listed with their size
// and keep the import incomplete until the owner decides about them.
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
			if filepath.Dir(path) != dir || !entry.Type().IsRegular() || !unifier.IsManagedSpecFilename(entry.Name()) {
				return nil
			}
			data, readErr := os.ReadFile(path) // #nosec G304 -- managed filename under the data directory.
			if readErr != nil {
				return readErr
			}
			report.UnownedSpecFiles++
			matched, archiveErr := archiveVersioned(ctx, pool, stacksDir+"/"+id+"/"+entry.Name(), data)
			if archiveErr != nil {
				return archiveErr
			}
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
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			rel, _ := filepath.Rel(dataDir, path)
			report.LegacyFiles++
			report.LegacyBytes += info.Size()
			listed = append(listed, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), info.Size()))
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

// archiveVersioned stores one file in the tenant-free archive (rows only
// added) and reads the stored bytes back by digest. A path that already holds
// archiveVersionLimit other versions is left alone and reported as not
// matching.
func archiveVersioned(ctx context.Context, pool *sql.DB, source string, data []byte) (bool, error) {
	digest := sha256Hex(data)
	var others int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM control_plane_disk_archive WHERE source_path = $1 AND sha256 <> $2`,
		source, digest).Scan(&others); err != nil {
		return false, err
	}
	if others >= archiveVersionLimit {
		return false, nil
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO control_plane_disk_archive (source_path, sha256, byte_size, content)
		VALUES ($1, $2, $3, $4) ON CONFLICT (source_path, sha256) DO NOTHING`, source, digest, len(data), data); err != nil {
		return false, err
	}
	var held string
	if err := pool.QueryRowContext(ctx, `SELECT encode(sha256(content), 'hex') FROM control_plane_disk_archive
		WHERE source_path = $1 AND sha256 = $2`, source, digest).Scan(&held); err != nil {
		return false, err
	}
	return held == digest, nil
}
