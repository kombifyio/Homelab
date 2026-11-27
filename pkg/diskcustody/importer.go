package diskcustody

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kombifyio/homelab/pkg/db"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/unifier"
)

// The control-plane files of the data directory that hold no tenant data.
// Monitoring history and runtime logs are history only and are not copied;
// the agent CA is unused while agent gRPC is off and its private key never
// leaves the disk. accountForRemainder covers the rest: stacks no tenant owns,
// legacy directories and entries this import does not know.
const (
	hostKeyFile    = "runtime-host-keys.json"
	knownHostsFile = "known_hosts"
	stacksDir      = "stacks"
	instanceFile   = "instance.json"
	enrollmentFile = "enrollment.json"
)

// ImportReport is the evidence of one import: what the disk holds and what
// the database holds for the same items afterwards.
type ImportReport struct {
	DataDir string

	HostKeyFileSHA256 string
	HostKeyPins       int
	HostKeyImported   int
	HostKeyMatching   int
	// HostKeyRetired counts disk pins the database retired: their managed
	// server is proven gone, so they are not imported again.
	HostKeyRetired   int
	HostKeyConflicts []string

	StacksOnDisk       int
	StacksMapped       int
	StackFilesOnDisk   int // managed spec files of mapped stacks
	StackFilesImported int
	StackFilesMatching int
	StackArchiveFiles  int // every regular file of mapped stacks
	StackArchiveBytes  int64
	StackArchiveMatch  int
	// StacksOrphaned name a tenant (spec metadata) under which no stacks row
	// exists: the stack was deleted. StacksUnresolved carry no tenant the
	// database confirms.
	StacksOrphaned   []string
	StacksUnresolved []string

	ArchiveFiles    int
	ArchiveBytes    int64
	ArchiveMatching int

	// StacksUnownedWithFiles names the unresolved and orphaned stacks that hold
	// any file but provider plugins (UnownedFiles, UnownedBytes): a live stack
	// may hide there, and only a tenant-scoped row is restored. Every file of
	// such a stack is copied into the tenant-free archive, so the disk's
	// content survives the disk; the managed spec files are counted
	// separately (UnownedSpecFiles, read back as UnownedSpecMatching).
	StacksUnownedWithFiles []string
	UnownedFiles           int
	UnownedBytes           int64
	// UnownedMatching counts the archived copies of an unowned stack's files
	// that were read back byte for byte.
	UnownedMatching     int
	UnownedSpecFiles    int
	UnownedSpecMatching int

	// LegacyFiles and LegacyBytes count what the legacy directories still hold
	// (copied into the tenant-free archive; LegacyMatching names the copies
	// read back byte for byte). UnknownEntries are top-level entries this
	// import does not know.
	LegacyFiles    int
	LegacyBytes    int64
	LegacyMatching int
	LegacyListing  string
	UnknownEntries []string

	Errors int
}

// Import copies the durable data-directory items into the runtime database.
// It never overwrites a database row: a pin or spec file the database already
// holds wins, and a disagreeing pin is reported as a conflict. It is safe to
// run on every boot while a disk is still mounted.
func Import(ctx context.Context, pool *sql.DB, dataDir string, log *slog.Logger) (ImportReport, error) {
	report := ImportReport{DataDir: dataDir}
	if pool == nil {
		return report, errors.New("disk custody import requires the runtime database")
	}
	if strings.TrimSpace(dataDir) == "" {
		return report, nil
	}
	if log == nil {
		log = slog.Default()
	}
	logInventory(dataDir, log)
	var errs []error
	if err := importHostKeys(ctx, pool, dataDir, &report); err != nil {
		errs = append(errs, fmt.Errorf("host key pins: %w", err))
	}
	if err := archiveControlPlaneFiles(ctx, pool, dataDir, &report); err != nil {
		errs = append(errs, fmt.Errorf("archive: %w", err))
	}
	if err := importStacks(ctx, pool, dataDir, &report); err != nil {
		errs = append(errs, fmt.Errorf("stacks: %w", err))
	}
	if err := accountForRemainder(ctx, pool, dataDir, &report); err != nil {
		errs = append(errs, fmt.Errorf("remainder: %w", err))
	}
	report.Errors = len(errs)
	log.Info("disk_custody_import",
		"data_dir", dataDir,
		"host_key_file_sha256", report.HostKeyFileSHA256,
		"host_key_pins", report.HostKeyPins,
		"host_key_imported", report.HostKeyImported,
		"host_key_matching", report.HostKeyMatching,
		"host_key_retired", report.HostKeyRetired,
		"host_key_conflicts", strings.Join(report.HostKeyConflicts, ","),
		"stacks_on_disk", report.StacksOnDisk,
		"stacks_mapped", report.StacksMapped,
		"stack_files_on_disk", report.StackFilesOnDisk,
		"stack_files_imported", report.StackFilesImported,
		"stack_files_matching", report.StackFilesMatching,
		"stack_archive_files", report.StackArchiveFiles,
		"stack_archive_bytes", report.StackArchiveBytes,
		"stack_archive_matching", report.StackArchiveMatch,
		"stacks_orphaned_n", len(report.StacksOrphaned),
		"stacks_orphaned", strings.Join(capNames(report.StacksOrphaned), ","),
		"stacks_unresolved_n", len(report.StacksUnresolved),
		"stacks_unresolved", strings.Join(capNames(report.StacksUnresolved), ","),
		"archive_files", report.ArchiveFiles,
		"archive_bytes", report.ArchiveBytes,
		"archive_matching", report.ArchiveMatching,
		"unowned_spec_files", report.UnownedSpecFiles,
		"unowned_spec_matching", report.UnownedSpecMatching,
		"stacks_unowned_with_files_n", len(report.StacksUnownedWithFiles),
		"stacks_unowned_with_files", strings.Join(capNames(report.StacksUnownedWithFiles), ","),
		"unowned_files", report.UnownedFiles,
		"unowned_bytes", report.UnownedBytes,
		"unowned_matching", report.UnownedMatching,
		"legacy_files", report.LegacyFiles,
		"legacy_bytes", report.LegacyBytes,
		"legacy_matching", report.LegacyMatching,
		"legacy_listing", report.LegacyListing,
		"unknown_entries", strings.Join(capNames(report.UnknownEntries), ","),
		"errors", report.Errors,
		"complete", report.Complete(),
	)
	return report, errors.Join(errs...)
}

// Complete reports whether the database holds every durable item of the disk
// byte for byte with no unowned, legacy, or unknown remainder requiring an
// owner decision before PR B disk deletion: every pin it has not retired, every
// managed spec file and an archive copy of every file of each stack the
// database knows, and an archive copy of the control-plane files. Even though
// unowned stack files and legacy directories are archived and verified by
// digest (UnownedMatching == UnownedFiles, LegacyMatching == LegacyFiles),
// Complete stays false while an unowned stack holds any file other than
// provider plugins (len(StacksUnownedWithFiles) > 0), while a legacy
// directory still holds a file (LegacyFiles > 0), or while the disk holds an
// unknown entry (len(UnknownEntries) > 0): each requires explicit owner
// approval before PR B removes the disk.
func (r ImportReport) Complete() bool {
	return r.Errors == 0 && len(r.HostKeyConflicts) == 0 && r.HostKeyMatching+r.HostKeyRetired == r.HostKeyPins &&
		r.StackFilesMatching == r.StackFilesOnDisk && r.StackArchiveMatch == r.StackArchiveFiles &&
		r.ArchiveMatching == r.ArchiveFiles && r.UnownedMatching == r.UnownedFiles &&
		r.UnownedSpecMatching == r.UnownedSpecFiles &&
		len(r.StacksUnownedWithFiles) == 0 && r.LegacyMatching == r.LegacyFiles &&
		r.LegacyFiles == 0 && len(r.UnknownEntries) == 0
}

// capNames bounds a name list written to one log line.
func capNames(names []string) []string {
	if len(names) <= legacyListLimit {
		return names
	}
	return append(append([]string{}, names[:legacyListLimit]...), fmt.Sprintf("...+%d", len(names)-legacyListLimit))
}

func logInventory(dataDir string, log *slog.Logger) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		log.Info("disk_custody_inventory", "data_dir", dataDir, "error", err.Error())
		return
	}
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		files, bytes := treeSize(filepath.Join(dataDir, entry.Name()))
		parts = append(parts, fmt.Sprintf("%s:%d:%d", entry.Name(), files, bytes))
	}
	log.Info("disk_custody_inventory", "data_dir", dataDir, "entries", strings.Join(parts, " "))
}

func treeSize(root string) (int, int64) {
	files, total := 0, int64(0)
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			files++
			total += info.Size()
		}
		return nil
	})
	return files, total
}

func importHostKeys(ctx context.Context, pool *sql.DB, dataDir string, report *ImportReport) error {
	path := filepath.Join(dataDir, hostKeyFile)
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed name in the data directory.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	report.HostKeyFileSHA256 = sha256Hex(raw)
	pins, err := jobs.ReadRuntimeHostKeyPinFile(path)
	if err != nil {
		return err
	}
	report.HostKeyPins = len(pins)
	store := HostKeyPins{DB: pool}
	before, err := store.LoadPins(ctx)
	if err != nil {
		return err
	}
	retired, err := store.RetiredPins(ctx, pins)
	if err != nil {
		return err
	}
	addresses := make([]string, 0, len(pins))
	for address := range pins {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		if retired[address] {
			report.HostKeyRetired++
			continue
		}
		held, err := store.pinFirst(ctx, address, pins[address], pinSourceDiskImport)
		if err != nil {
			return err
		}
		if _, existed := before[address]; !existed {
			report.HostKeyImported++
		}
		if held == pins[address] {
			report.HostKeyMatching++
		} else {
			report.HostKeyConflicts = append(report.HostKeyConflicts, address)
		}
	}
	return nil
}

// archiveControlPlaneFiles copies the tenant-free control-plane files.
func archiveControlPlaneFiles(ctx context.Context, pool *sql.DB, dataDir string, report *ImportReport) error {
	for _, name := range []string{enrollmentFile, instanceFile, hostKeyFile, knownHostsFile} {
		info, err := os.Lstat(filepath.Join(dataDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dataDir, name)) // #nosec G304 -- fixed name in the data directory.
		if err != nil {
			return err
		}
		report.ArchiveFiles++
		report.ArchiveBytes += int64(len(data))
		matched, err := archiveVersioned(ctx, pool, name, data)
		if err != nil {
			return err
		}
		if matched {
			report.ArchiveMatching++
		}
	}
	return nil
}

type diskStack struct {
	id       string
	dir      string
	managed  map[string][]byte
	evidence []string // tenant candidates from the stack's own spec metadata
}

func importStacks(ctx context.Context, pool *sql.DB, dataDir string, report *ImportReport) error {
	root := filepath.Join(dataDir, stacksDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pending := map[string]*diskStack{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		report.StacksOnDisk++
		stack := &diskStack{id: entry.Name(), dir: filepath.Join(root, entry.Name()), managed: map[string][]byte{}}
		files, err := os.ReadDir(stack.dir)
		if err != nil {
			return err
		}
		for _, file := range files {
			if !file.Type().IsRegular() || !unifier.IsManagedSpecFilename(file.Name()) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(stack.dir, file.Name())) // #nosec G304 -- managed filename in the data directory.
			if err != nil {
				return err
			}
			stack.managed[file.Name()] = data
		}
		stack.evidence = tenantEvidence(stack.managed)
		pending[stack.id] = stack
	}
	// First the tenant each stack names itself, then every tenant that holds
	// a stack (migration 207). Each match is a tenant-scoped stacks row.
	orphanCandidates := map[string]bool{}
	for id, stack := range pending {
		for _, tenantID := range stack.evidence {
			owned, err := tenantStacks(ctx, pool, tenantID, []string{id})
			if err != nil {
				return err
			}
			if len(owned) == 1 {
				if err := importStack(ctx, pool, tenantID, stack, report); err != nil {
					return fmt.Errorf("stack %s: %w", id, err)
				}
				delete(pending, id)
				break
			}
		}
		if _, still := pending[id]; still && len(stack.evidence) > 0 {
			orphanCandidates[id] = true
		}
	}
	if len(pending) > 0 {
		tenants, err := listTenants(ctx, pool)
		if err != nil {
			return err
		}
		for _, tenantID := range tenants {
			if len(pending) == 0 {
				break
			}
			ids := make([]string, 0, len(pending))
			for id := range pending {
				ids = append(ids, id)
			}
			owned, err := tenantStacks(ctx, pool, tenantID, ids)
			if err != nil {
				return err
			}
			for _, id := range owned {
				if err := importStack(ctx, pool, tenantID, pending[id], report); err != nil {
					return fmt.Errorf("stack %s: %w", id, err)
				}
				delete(pending, id)
			}
		}
	}
	for id := range pending {
		if orphanCandidates[id] {
			report.StacksOrphaned = append(report.StacksOrphaned, id)
		} else {
			report.StacksUnresolved = append(report.StacksUnresolved, id)
		}
	}
	sort.Strings(report.StacksOrphaned)
	sort.Strings(report.StacksUnresolved)
	return nil
}

// tenantEvidence reads the tenant and owner the stack's intent records.
func tenantEvidence(managed map[string][]byte) []string {
	var intent struct {
		Metadata map[string]string `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(managed[unifier.IntentSpecFilename], &intent); err != nil {
		return nil
	}
	var out []string
	for _, key := range []string{"tenant_id", "owner_id"} {
		if value := strings.TrimSpace(intent.Metadata[key]); value != "" && !contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// listTenants pages the secret-free directory of the tenants that hold a
// stack (migration 207). The stacks rows themselves stay behind tenant RLS:
// the directory function reads them under the migration role and returns
// tenant identifiers alone.
//
// A binary that runs before its migration falls back to the directory the
// drift schedule kept (migration 153), so the import keeps its previous
// behavior instead of failing the boot.
func listTenants(ctx context.Context, pool *sql.DB) ([]string, error) {
	tenants, err := listStackTenantDirectory(ctx, pool)
	if err == nil {
		return tenants, nil
	}
	if !errors.Is(err, errStackTenantDirectoryPending) {
		return nil, err
	}
	return listDriftScheduleTenants(ctx, pool)
}

// listDriftScheduleTenants pages the tenant directory of the drift schedule.
func listDriftScheduleTenants(ctx context.Context, pool *sql.DB) ([]string, error) {
	return pageTenants(ctx, pool, `SELECT tenant_id FROM public.techstack_list_drift_schedule_tenants($1, $2)`)
}

// pageTenants reads one bounded page of tenant identifiers at a time.
func pageTenants(ctx context.Context, pool *sql.DB, query string) ([]string, error) {
	const page = 100
	var tenants []string
	after := ""
	for {
		rows, err := pool.QueryContext(ctx, query, after, page)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			tenants = append(tenants, id)
			after = id
			count++
		}
		iterErr := rows.Err()
		if closeErr := rows.Close(); iterErr != nil || closeErr != nil {
			return nil, errors.Join(iterErr, closeErr)
		}
		if count < page {
			return tenants, nil
		}
	}
}

func tenantStacks(ctx context.Context, pool *sql.DB, tenantID string, ids []string) ([]string, error) {
	tx, err := db.BeginTenantTransaction(ctx, pool, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM stacks WHERE tenant_id = $1 AND id = ANY($2::text[])`, tenantID, textArray(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owned []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		owned = append(owned, id)
	}
	return owned, rows.Err()
}

// importStack copies the managed spec files into stack_spec_files and every
// regular file of the stack directory into the tenant's stack_disk_archive,
// then reads both back.
func importStack(ctx context.Context, pool *sql.DB, tenantID string, stack *diskStack, report *ImportReport) error {
	var archive []string
	if err := filepath.WalkDir(stack.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Provider plugins are downloads OpenTofu restores itself, not state.
		if entry.IsDir() && entry.Name() == ".terraform" {
			return filepath.SkipDir
		}
		if entry.Type().IsRegular() {
			rel, relErr := filepath.Rel(stack.dir, path)
			if relErr != nil {
				return relErr
			}
			archive = append(archive, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(archive)
	tx, err := db.BeginTenantTransaction(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for filename, data := range stack.managed {
		result, err := tx.ExecContext(ctx, `INSERT INTO stack_spec_files (tenant_id, stack_id, filename, content, sha256, source)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (tenant_id, stack_id, filename) DO NOTHING`,
			tenantID, stack.id, filename, data, sha256Hex(data), specSourceDiskImport)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			report.StackFilesImported++
		}
	}
	digests := map[string]string{}
	for _, rel := range archive {
		data, err := os.ReadFile(filepath.Join(stack.dir, filepath.FromSlash(rel))) // #nosec G304 -- walked inside the stack directory.
		if err != nil {
			return err
		}
		digest := sha256Hex(data)
		digests[rel] = digest
		report.StackArchiveBytes += int64(len(data))
		if _, err := tx.ExecContext(ctx, `INSERT INTO stack_disk_archive (tenant_id, stack_id, relative_path, sha256, byte_size, content)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (tenant_id, stack_id, relative_path, sha256) DO NOTHING`,
			tenantID, stack.id, rel, digest, len(data), data); err != nil {
			return err
		}
	}
	held, err := heldDigests(ctx, tx, `SELECT filename, sha256 FROM stack_spec_files WHERE tenant_id = $1 AND stack_id = $2`, tenantID, stack.id)
	if err != nil {
		return err
	}
	archived, err := heldDigests(ctx, tx, `SELECT relative_path || '@' || sha256, encode(sha256(content), 'hex') FROM stack_disk_archive WHERE tenant_id = $1 AND stack_id = $2`, tenantID, stack.id)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	report.StacksMapped++
	report.StackFilesOnDisk += len(stack.managed)
	for filename, data := range stack.managed {
		if held[filename] == sha256Hex(data) {
			report.StackFilesMatching++
		}
	}
	report.StackArchiveFiles += len(archive)
	for rel, digest := range digests {
		if archived[rel+"@"+digest] == digest {
			report.StackArchiveMatch++
		}
	}
	return nil
}

func heldDigests(ctx context.Context, tx *sql.Tx, query, tenantID, stackID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, query, tenantID, stackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string]string{}
	for rows.Next() {
		var key, digest string
		if err := rows.Scan(&key, &digest); err != nil {
			return nil, err
		}
		held[key] = digest
	}
	return held, rows.Err()
}

// textArray renders a Postgres text[] literal for the stdlib driver.
func textArray(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)+`"`)
	}
	return "{" + strings.Join(quoted, ",") + "}"
}
