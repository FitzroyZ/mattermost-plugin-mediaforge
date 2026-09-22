package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
)

// The store check was written because the panel reported the spool and called
// it storage. These tests hold the distinction in place: the thresholds, the S3
// case, and the relative-path resolution are each a way of reporting on the
// wrong thing, which is the defect being fixed.

func TestResolveStoreDir(t *testing.T) {
	cases := []struct {
		name       string
		dir        string
		serverRoot string
		want       string
	}{
		{
			name:       "the conventional relative default resolves against the server root",
			dir:        "./data/",
			serverRoot: "/opt/mattermost",
			want:       "/opt/mattermost/data",
		},
		{
			name:       "an absolute path is left alone",
			dir:        "/mnt/usb/mattermost-data",
			serverRoot: "/opt/mattermost",
			want:       "/mnt/usb/mattermost-data",
		},
		{
			name:       "an empty setting means the documented default",
			dir:        "",
			serverRoot: "/opt/mattermost",
			want:       "/opt/mattermost/data",
		},
		{
			name:       "surrounding whitespace does not create a different path",
			dir:        "  ./data/  ",
			serverRoot: "/opt/mattermost",
			want:       "/opt/mattermost/data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStoreDir(tc.dir, tc.serverRoot)
			if got != tc.want {
				t.Fatalf("resolveStoreDir(%q, %q) = %q, want %q",
					tc.dir, tc.serverRoot, got, tc.want)
			}
		})
	}
}

// A relative path must NOT be resolved against the plugin's own working
// directory. That is the failure the server-root argument exists to prevent:
// it would report on a path that almost certainly does not exist, or worse, on
// a different directory that does.
func TestResolveStoreDirDoesNotUsePluginWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	got := resolveStoreDir("./data/", "/opt/mattermost")
	if strings.HasPrefix(got, cwd) {
		t.Fatalf("resolved to %q, which is under the plugin's working directory %q — "+
			"the store is relative to the SERVER root", got, cwd)
	}
}

func TestCheckStoreThresholds(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name       string
		free       uint64
		wantStatus healthStatus
		wantIn     string
	}{
		{
			name:       "plenty of room is green",
			free:       500 << 30,
			wantStatus: healthOK,
			wantIn:     "free",
		},
		{
			name:       "below the warning threshold is yellow",
			free:       5 << 30,
			wantStatus: healthWarn,
			wantIn:     "nothing deletes them",
		},
		{
			name:       "nearly full is red",
			free:       500 << 20,
			wantStatus: healthError,
			wantIn:     "will start failing",
		},
		{
			name:       "exactly at the warning threshold is still green",
			free:       storeWarnFreeBytes,
			wantStatus: healthOK,
			wantIn:     "free",
		},
		{
			name:       "exactly at the error threshold is yellow, not red",
			free:       storeErrorFreeBytes,
			wantStatus: healthWarn,
			wantIn:     "free",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := checkStore(
				fileStoreSettings{DriverName: "local", Directory: dir},
				"", func(string) (uint64, error) { return tc.free, nil })

			if c.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", c.Status, tc.wantStatus, c.Detail)
			}
			if !strings.Contains(c.Detail, tc.wantIn) {
				t.Fatalf("detail %q does not mention %q", c.Detail, tc.wantIn)
			}
			if !strings.Contains(c.Detail, dir) {
				t.Fatalf("detail %q does not name the store directory %q", c.Detail, dir)
			}
		})
	}
}

// An S3 store has no local disk. Reporting free bytes for the configured
// directory would be the same mistake as reporting the spool: a number that
// looks authoritative and describes something else.
func TestCheckStoreDoesNotInventFiguresForRemoteDrivers(t *testing.T) {
	called := false
	c := checkStore(
		fileStoreSettings{DriverName: "amazons3", Directory: "/opt/mattermost/data"},
		"", func(string) (uint64, error) {
			called = true
			return 1 << 20, nil
		})

	if called {
		t.Fatal("free space was measured for an S3 store — the local path is not the store")
	}
	if c.Status != healthOK {
		t.Fatalf("status = %q, want ok for a remote driver", c.Status)
	}
	if !strings.Contains(c.Detail, "amazons3") {
		t.Fatalf("detail %q does not say which driver is in use", c.Detail)
	}
	for _, unwanted := range []string{"free.", "GB free"} {
		if strings.Contains(c.Detail, unwanted) {
			t.Fatalf("detail %q reports free space for a store that is not on this disk", c.Detail)
		}
	}
}

// A store path that does not exist is an ERROR, not a warning: it usually means
// a disk is not mounted, and every upload is about to fail.
func TestCheckStoreReportsMissingDirectoryAsError(t *testing.T) {
	c := checkStore(
		fileStoreSettings{DriverName: "local", Directory: "/definitely/not/here"},
		"", func(string) (uint64, error) { return 500 << 30, nil })

	if c.Status != healthError {
		t.Fatalf("status = %q, want error for a missing store directory", c.Status)
	}
	if !strings.Contains(c.Detail, "uploads will fail") &&
		!strings.Contains(c.Detail, "uploads are kept") {
		t.Fatalf("detail %q does not explain the consequence", c.Detail)
	}
}

func TestCheckStoreReportsFileInsteadOfDirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := checkStore(
		fileStoreSettings{DriverName: "local", Directory: f},
		"", func(string) (uint64, error) { return 500 << 30, nil })

	if c.Status != healthError {
		t.Fatalf("status = %q, want error when the store path is a file", c.Status)
	}
}

// If free space cannot be read the check must say so rather than claim health.
func TestCheckStoreDoesNotClaimHealthWhenSpaceIsUnknown(t *testing.T) {
	c := checkStore(
		fileStoreSettings{DriverName: "local", Directory: t.TempDir()},
		"", func(string) (uint64, error) { return 0, errors.New("statfs refused") })

	if c.Status != healthWarn {
		t.Fatalf("status = %q, want warn when free space is unknown", c.Status)
	}
	if !strings.Contains(c.Detail, "could not be determined") {
		t.Fatalf("detail %q does not say the figure is unavailable", c.Detail)
	}
}

// An empty driver name means the local default; it must be measured, not
// treated as a remote store and skipped.
func TestCheckStoreTreatsEmptyDriverAsLocal(t *testing.T) {
	called := false
	c := checkStore(
		fileStoreSettings{DriverName: "", Directory: t.TempDir()},
		"", func(string) (uint64, error) {
			called = true
			return 500 << 30, nil
		})

	if !called {
		t.Fatal("an empty driver name was treated as remote; the default store is local")
	}
	if c.Status != healthOK {
		t.Fatalf("status = %q, want ok", c.Status)
	}
}

// serverConfigWithStore builds the slice of the server configuration the store
// check reads. A helper rather than four lines in every test, because the
// pointer fields are easy to get subtly wrong.
func serverConfigWithStore(driver, directory string) *model.Config {
	cfg := &model.Config{}
	cfg.FileSettings.DriverName = model.NewPointer(driver)
	cfg.FileSettings.Directory = model.NewPointer(directory)
	return cfg
}

// The store check must be a SECOND line, not a replacement: spool and store
// answer different questions and both belong in the report.
func TestHealthReportCoversSpoolAndStoreSeparately(t *testing.T) {
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{
			version:  "ffmpeg version 7.1.5",
			encoders: " V....D libx264 H.264\n",
		}, t.TempDir())

	api := &plugintest.API{}
	api.On("GetConfig").Return(serverConfigWithStore("local", t.TempDir())).Maybe()
	p.SetAPI(api)

	report := p.healthReport()

	var haveSpool, haveStore bool
	for _, c := range report.Checks {
		switch c.ID {
		case "spool":
			haveSpool = true
		case "store":
			haveStore = true
		}
	}
	if !haveSpool {
		t.Error("the spool check disappeared; a transcode still needs working space")
	}
	if !haveStore {
		t.Error("no store check in the report — the panel would still describe the wrong disk")
	}
}
