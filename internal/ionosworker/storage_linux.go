package ionosworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const quotaPath = "/usr/sbin/xfs_quota"

func checkStorage(ctx context.Context, slots int) error {
	mount, err := command(ctx, "/usr/bin/findmnt", "--mountpoint", dataDirectory, "--noheadings", "--output", "FSTYPE")
	if err != nil || strings.TrimSpace(string(mount)) != "xfs" {
		return errors.New("worker requires a dedicated XFS data mount")
	}
	state, err := command(ctx, quotaPath, "-x", "-c", "state -p", dataDirectory)
	if err != nil || !strings.Contains(string(state), "Accounting: ON\n") || !strings.Contains(string(state), "Enforcement: ON\n") {
		return errors.New("worker requires XFS project quota accounting and enforcement")
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(dataDirectory, &filesystem); err != nil || filesystem.Blocks*uint64(filesystem.Bsize) < uint64(2*slots+4)<<30 {
		return errors.New("worker data mount cannot hold admitted slot quotas and reserve")
	}
	return nil
}

func generationProject(slot int) string { return strconv.Itoa(10000 + slot) }

func configureStorage(ctx context.Context, record *generation, volume string) error {
	parent := filepath.Dir(volume)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	project := generationProject(record.Slot)
	if _, err := command(ctx, quotaPath, "-x", "-c", "limit -p bsoft=0 bhard=2g isoft=0 ihard=65536 "+project, dataDirectory); err != nil {
		return err
	}
	if _, err := command(ctx, quotaPath, "-x", "-c", "project -s -d 0 -p "+parent+" "+project, dataDirectory); err != nil {
		return err
	}
	if err := os.MkdirAll(volume, 0700); err != nil {
		return err
	}
	return checkVolumeQuota(ctx, record.Slot, volume)
}

func checkVolumeQuota(ctx context.Context, slot int, volume string) error {
	project := generationProject(slot)
	for _, directory := range []string{filepath.Dir(volume), volume} {
		data, err := command(ctx, quotaPath, "-x", "-c", "project -c -d 0 -p "+directory+" "+project, dataDirectory)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if err != nil || len(lines) != 2 || lines[0] != "Checking project "+project+" (path "+directory+")..." || !strings.HasPrefix(lines[1], "Processed 1 ") {
			return errors.New("worker volume quota inheritance unconfirmed")
		}
	}
	for _, limit := range []struct{ metric, hard string }{{"b", "2097152"}, {"i", "65536"}} {
		data, err := command(ctx, quotaPath, "-x", "-c", "report -p -n -N -"+limit.metric+" -L "+project+" -U "+project, dataDirectory)
		fields := strings.Fields(string(data))
		if err != nil || len(fields) < 4 || fields[0] != "#"+project || fields[2] != "0" || fields[3] != limit.hard {
			return errors.New("worker volume hard quota readback unconfirmed")
		}
	}
	return nil
}
