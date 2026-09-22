package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The store check exists because the health panel reported the WRONG DISK.
//
// the health panel work added a spool check: free space where a file sits for seconds
// during transcoding. a reviewer asked the right question — that is the
// working directory, not where the media end up. He was right, and the gap is
// worse than a missing figure.
//
// On one production server the two were different devices:
//
//	/tmp (spool)             on the container root   16 GB,   12.8 GB free
//	/opt/mattermost/data     on a 1 TB volume         1 TB, 1022 GB free
//
// A panel that reports only the spool can read "12.8 GB free" while the disk
// holding every upload fills up. Uploads then fail and the panel is still
// green. A monitoring display that stays green through the outage it exists to
// prevent is worse than no display at all.
//
// This adds the store as a SECOND check rather than replacing the spool: they
// answer different questions. The spool is about whether a transcode can run at
// all; the store is about whether uploads can be kept, and it is the one that
// fills up slowly enough to be caught in advance.
//
// What it deliberately does NOT do:
//
//   - It does not touch the S3 case. FileSettings.DriverName can be "amazons3",
//     and then no local path means anything. Reporting free bytes for a
//     directory that is not the store would be the same class of error this
//     check was written to fix, so S3 is reported as "not a local disk" and no
//     figure is invented.
//   - It does not walk the store to total the media. On a store with a million
//     files that is minutes of disk I/O on a page load, and the figure is
//     already in the filesystem's own accounting.

// storeWarnFreeBytes is when the store check turns yellow.
//
// Bigger than the spool threshold on purpose. Running out of spool space fails
// one upload and is over; running out of STORE space fails every upload from
// then on and cannot be undone by waiting. Ten gigabytes on a store sized for
// years of uploads is roughly a week of warning on a busy server — enough time
// to act without being alarmist.
const storeWarnFreeBytes = 10 << 30

// storeErrorFreeBytes is when it turns red: uploads are about to start failing.
const storeErrorFreeBytes = 1 << 30

// resolveStoreDir turns the configured directory into an absolute path.
//
// FileSettings.Directory is conventionally "./data/", relative to the server's
// working directory — which is where the Mattermost binary was started, not
// where the plugin's process happens to be. Resolving it against the plugin's
// own working directory would silently report on a different, probably
// non-existent, path.
func resolveStoreDir(dir, serverRoot string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = "./data/"
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if serverRoot == "" {
		// Nothing better available: clean it and let the caller report that
		// the path does not exist, rather than guess at a root.
		return filepath.Clean(dir)
	}
	return filepath.Clean(filepath.Join(serverRoot, dir))
}

// fileStoreSettings is the part of the server configuration this check needs.
//
// A narrow struct rather than the server's own type: the plugin only ever reads
// these two fields, and depending on the full model here would drag the check
// into every future change of an unrelated setting.
type fileStoreSettings struct {
	// DriverName is "local" or "amazons3".
	DriverName string
	// Directory is where a local store keeps its files.
	Directory string
}

// checkStore reports where uploads are kept and whether there is room left.
func checkStore(fs fileStoreSettings, serverRoot string, free freeSpaceFunc) healthCheck {
	c := healthCheck{ID: "store", Label: "File store"}

	driver := strings.TrimSpace(fs.DriverName)
	if driver != "" && driver != "local" {
		// S3 and friends: there is no local disk to measure, and inventing a
		// number for one would be the exact error this check exists to fix.
		c.Status = healthOK
		c.Detail = fmt.Sprintf("uploads are stored through the %q driver, not on a "+
			"local disk. Free space is reported by that service, not here.", driver)
		return c
	}

	dir := resolveStoreDir(fs.Directory, serverRoot)

	info, err := os.Stat(dir)
	if err != nil {
		c.Status = healthError
		c.Detail = fmt.Sprintf("%s could not be read (%v). This is where uploads are "+
			"kept; if the path is wrong or a disk is not mounted, uploads will fail.", dir, err)
		return c
	}
	if !info.IsDir() {
		c.Status = healthError
		c.Detail = fmt.Sprintf("%s is not a directory. Uploads are kept there; "+
			"check FileSettings.Directory in the server configuration.", dir)
		return c
	}

	avail, err := free(dir)
	if err != nil {
		c.Status = healthWarn
		c.Detail = fmt.Sprintf("%s — free space could not be determined (%v).", dir, err)
		return c
	}

	switch {
	case avail < storeErrorFreeBytes:
		c.Status = healthError
		c.Detail = fmt.Sprintf("%s — only %s free. Uploads will start failing; "+
			"free space or move the file store to a larger disk.",
			dir, humanBytes(avail))
	case avail < storeWarnFreeBytes:
		c.Status = healthWarn
		c.Detail = fmt.Sprintf("%s — %s free. This is where every upload is kept, "+
			"and nothing deletes them on its own.", dir, humanBytes(avail))
	default:
		c.Status = healthOK
		c.Detail = fmt.Sprintf("%s — %s free.", dir, humanBytes(avail))
	}
	return c
}

// freeSpaceFunc is the free-space lookup, injected so the thresholds can be
// tested without filling a real disk.
type freeSpaceFunc func(string) (uint64, error)
