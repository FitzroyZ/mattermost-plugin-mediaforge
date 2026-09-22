package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

// repoManifestPath points at the real manifest from this test's directory.
const repoManifestPath = "../../plugin.json"

// expectedPluginID is fixed by SPEC.md and must never change silently: the
// plugin ID is the identity Mattermost stores per installation.
const expectedPluginID = "com.github.berzionline.mediaforge"

// TestRepositoryManifest asserts that the manifest shipped in this repository
// is valid JSON, carries the agreed plugin ID, and satisfies the Mattermost
// manifest requirements for a server-side plugin.
func TestRepositoryManifest(t *testing.T) {
	data, err := os.ReadFile(repoManifestPath)
	if err != nil {
		t.Fatalf("cannot read %s: %v", repoManifestPath, err)
	}

	if !json.Valid(data) {
		t.Fatalf("%s is not valid JSON", repoManifestPath)
	}

	m, err := loadManifest(repoManifestPath)
	if err != nil {
		t.Fatalf("cannot load manifest: %v", err)
	}

	if m.Id != expectedPluginID {
		t.Errorf("plugin ID = %q, want %q", m.Id, expectedPluginID)
	}

	if err := validateManifest(m); err != nil {
		t.Errorf("manifest failed validation: %v", err)
	}

	// Every declared executable path must sit inside the bundle's server
	// directory; Mattermost resolves them relative to the manifest.
	for osArch, path := range m.Server.Executables {
		if filepath.IsAbs(path) {
			t.Errorf("executable path for %s is absolute: %q", osArch, path)
		}
		if filepath.Dir(path) != "server/dist" {
			t.Errorf("executable path for %s = %q, want it under server/dist", osArch, path)
		}
	}

	if _, ok := m.Server.Executables["linux-amd64"]; !ok {
		t.Error("manifest declares no linux-amd64 executable")
	}
}

// TestValidateManifestRejectsBrokenManifests proves the validation above can
// actually fail. Without this, a validator that returns nil unconditionally
// would look just as green as a correct one.
func TestValidateManifestRejectsBrokenManifests(t *testing.T) {
	valid := func() *model.Manifest {
		m, err := loadManifest(repoManifestPath)
		if err != nil {
			t.Fatalf("cannot load manifest: %v", err)
		}
		return m
	}

	cases := map[string]func(*model.Manifest){
		"empty id":            func(m *model.Manifest) { m.Id = "" },
		"illegal id":          func(m *model.Manifest) { m.Id = "not a valid id!" },
		"empty name":          func(m *model.Manifest) { m.Name = "" },
		"non-semver version":  func(m *model.Manifest) { m.Version = "1.0" },
		"no version":          func(m *model.Manifest) { m.Version = "" },
		"no min server":       func(m *model.Manifest) { m.MinServerVersion = "" },
		"no server component": func(m *model.Manifest) { m.Server = nil },
		"no executables": func(m *model.Manifest) {
			m.Server.Executables = nil
			m.Server.Executable = ""
		},
		"empty executable path": func(m *model.Manifest) {
			m.Server.Executables["linux-amd64"] = ""
		},
		"invalid homepage url": func(m *model.Manifest) { m.HomepageURL = "not-a-url" },
		"absolute executable path": func(m *model.Manifest) {
			m.Server.Executables["linux-amd64"] = "/usr/bin/plugin"
		},
	}

	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid()
			breakIt(m)
			if err := validateManifest(m); err == nil {
				t.Errorf("validateManifest accepted a manifest broken by %q", name)
			}
		})
	}
}

// TestCheckBundle covers the staged-bundle check: a directory that holds the
// manifest and every declared executable passes, and each way of being
// incomplete fails.
func TestCheckBundle(t *testing.T) {
	m, err := loadManifest(repoManifestPath)
	if err != nil {
		t.Fatalf("cannot load manifest: %v", err)
	}

	// stage builds a bundle directory. Callers mutate it afterwards to
	// produce the broken variants.
	stage := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("cannot stage manifest: %v", err)
		}
		for _, path := range m.Server.Executables {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("cannot stage directory: %v", err)
			}
			if err := os.WriteFile(full, []byte("binary"), 0o755); err != nil {
				t.Fatalf("cannot stage executable: %v", err)
			}
		}
		// The webapp bundle is staged from the manifest too, so this helper
		// keeps describing "a complete bundle" rather than "a bundle complete
		// as of the day it was written".
		if m.Webapp != nil && m.Webapp.BundlePath != "" {
			full := filepath.Join(dir, m.Webapp.BundlePath)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("cannot stage the webapp directory: %v", err)
			}
			if err := os.WriteFile(full, []byte("(function(){}());"), 0o644); err != nil {
				t.Fatalf("cannot stage the webapp bundle: %v", err)
			}
		}
		return dir
	}

	t.Run("complete bundle passes", func(t *testing.T) {
		if err := checkBundle(m, stage(t)); err != nil {
			t.Errorf("checkBundle rejected a complete bundle: %v", err)
		}
	})

	t.Run("missing manifest fails", func(t *testing.T) {
		dir := stage(t)
		if err := os.Remove(filepath.Join(dir, "plugin.json")); err != nil {
			t.Fatalf("cannot remove manifest: %v", err)
		}
		if err := checkBundle(m, dir); err == nil {
			t.Error("checkBundle accepted a bundle without plugin.json")
		}
	})

	t.Run("missing executable fails", func(t *testing.T) {
		dir := stage(t)
		if err := os.Remove(filepath.Join(dir, m.Server.Executables["linux-amd64"])); err != nil {
			t.Fatalf("cannot remove executable: %v", err)
		}
		if err := checkBundle(m, dir); err == nil {
			t.Error("checkBundle accepted a bundle without the linux-amd64 executable")
		}
	})

	t.Run("empty executable fails", func(t *testing.T) {
		dir := stage(t)
		path := filepath.Join(dir, m.Server.Executables["linux-amd64"])
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatalf("cannot truncate executable: %v", err)
		}
		if err := checkBundle(m, dir); err == nil {
			t.Error("checkBundle accepted a bundle with an empty executable")
		}
	})

	// The webapp bundle gets the same two checks as an executable, because it
	// fails the same way and worse: the manifest declares it, so the server
	// serves a 404 to every browser that loads the plugin, and the symptom is
	// "the record button is not there" with nothing in the log to explain it.
	t.Run("missing webapp bundle fails", func(t *testing.T) {
		if m.Webapp == nil || m.Webapp.BundlePath == "" {
			t.Skip("the manifest declares no webapp bundle")
		}
		dir := stage(t)
		if err := os.Remove(filepath.Join(dir, m.Webapp.BundlePath)); err != nil {
			t.Fatalf("cannot remove the webapp bundle: %v", err)
		}
		if err := checkBundle(m, dir); err == nil {
			t.Error("checkBundle accepted a bundle whose declared webapp bundle is absent")
		}
	})

	t.Run("empty webapp bundle fails", func(t *testing.T) {
		if m.Webapp == nil || m.Webapp.BundlePath == "" {
			t.Skip("the manifest declares no webapp bundle")
		}
		dir := stage(t)
		if err := os.WriteFile(filepath.Join(dir, m.Webapp.BundlePath), nil, 0o644); err != nil {
			t.Fatalf("cannot truncate the webapp bundle: %v", err)
		}
		if err := checkBundle(m, dir); err == nil {
			t.Error("checkBundle accepted a bundle with an empty webapp bundle")
		}
	})
}
