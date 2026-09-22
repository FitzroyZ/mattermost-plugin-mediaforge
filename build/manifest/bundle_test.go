package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

// bundleGlob matches the archive produced by `make dist`, relative to this
// test's directory.
const bundleGlob = "../../dist/*.tar.gz"

// TestBuiltBundleIsAValidPlugin unpacks the archive produced by `make dist` and
// checks it the way a Mattermost server does when a plugin is uploaded: find
// the manifest in the extracted tree with model.FindManifest, validate it, and
// confirm the executable it promises for this platform is actually there.
//
// The test skips when no archive has been built, so `go test ./...` stays
// usable on its own; CI runs it after `make dist`.
func TestBuiltBundleIsAValidPlugin(t *testing.T) {
	matches, err := filepath.Glob(bundleGlob)
	if err != nil {
		t.Fatalf("cannot search for a bundle: %v", err)
	}
	if len(matches) == 0 {
		t.Skipf("no bundle found at %s — run `make dist` first", bundleGlob)
	}

	bundle := matches[0]
	root := t.TempDir()
	extractTarGz(t, bundle, root)

	// Mattermost unpacks the archive and expects the manifest either at the
	// root or one directory down. Mirror that lookup.
	dir := root
	if _, _, err := model.FindManifest(dir); err != nil {
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			t.Fatalf("cannot read extracted bundle: %v", rerr)
		}
		if len(entries) != 1 || !entries[0].IsDir() {
			t.Fatalf("no manifest at the archive root and no single top-level directory to look in")
		}
		dir = filepath.Join(root, entries[0].Name())
	}

	m, path, err := model.FindManifest(dir)
	if err != nil {
		t.Fatalf("Mattermost could not find a manifest in the bundle: %v", err)
	}
	t.Logf("found manifest at %s", path)

	if m.Id != expectedPluginID {
		t.Errorf("bundled plugin ID = %q, want %q", m.Id, expectedPluginID)
	}

	if err := validateManifest(m); err != nil {
		t.Errorf("bundled manifest failed validation: %v", err)
	}

	if err := checkBundle(m, dir); err != nil {
		t.Errorf("bundle is incomplete: %v", err)
	}

	// The archive directory carries the plugin ID, which is the name the
	// server uses for the installed plugin directory.
	if dir != root && filepath.Base(dir) != m.Id {
		t.Errorf("bundle directory = %q, want the plugin ID %q", filepath.Base(dir), m.Id)
	}
}

// extractTarGz unpacks a gzipped tar archive into dest. Entries that would
// escape dest are rejected rather than written, so a malformed archive cannot
// scribble outside the temporary directory.
func extractTarGz(t *testing.T, archive, dest string) {
	t.Helper()

	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("cannot open %s: %v", archive, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not a valid gzip archive: %v", archive, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("%s is not a valid tar archive: %v", archive, err)
		}

		target := filepath.Join(dest, filepath.Clean(header.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			t.Fatalf("archive entry %q escapes the extraction directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("cannot create %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("cannot create %s: %v", filepath.Dir(target), err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o777)
			if err != nil {
				t.Fatalf("cannot write %s: %v", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				t.Fatalf("cannot write %s: %v", target, err)
			}
			out.Close()
		default:
			t.Fatalf("unexpected entry type %v for %q — a plugin bundle should only contain files and directories", header.Typeflag, header.Name)
		}
	}
}
