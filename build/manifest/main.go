// Command manifest reads plugin.json, validates it against the Mattermost
// manifest requirements, and either prints a single field or checks a staged
// bundle directory against the manifest.
//
// It exists so the Makefile and CI can work with manifest values without
// depending on jq or any tool beyond the Go toolchain the build already
// requires, and so that a broken manifest or an incomplete bundle fails the
// build instead of producing an archive Mattermost cannot load.
//
// Usage:
//
//	manifest id                    print the plugin ID
//	manifest version               print the plugin version
//	manifest validate              validate plugin.json
//	manifest check-bundle <dir>    validate plugin.json and verify that every
//	                               declared executable exists in <dir>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattermost/mattermost/server/public/model"
)

// manifestPath is the manifest location relative to the repository root, which
// is the working directory the Makefile and CI use.
const manifestPath = "plugin.json"

const usage = "usage: manifest <id|version|validate|check-bundle <dir>>"

// loadManifest reads and parses a plugin manifest from disk.
func loadManifest(path string) (*model.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	var m model.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	return &m, nil
}

// validateManifest checks the manifest against what Mattermost requires from a
// server-side plugin bundle. model.Manifest.IsValid covers the generic rules
// (plugin ID shape, name, semver versions, URLs); the checks below cover what
// this plugin specifically needs in order to be installable and loadable.
func validateManifest(m *model.Manifest) error {
	if err := m.IsValid(); err != nil {
		return fmt.Errorf("manifest is not valid: %w", err)
	}

	if m.Version == "" {
		return fmt.Errorf("manifest has no version")
	}

	if m.MinServerVersion == "" {
		return fmt.Errorf("manifest has no min_server_version")
	}

	if !m.HasServer() {
		return fmt.Errorf("manifest declares no server component")
	}

	if len(m.Server.Executables) == 0 && m.Server.Executable == "" {
		return fmt.Errorf("manifest declares no server executable")
	}

	for osArch, path := range m.Server.Executables {
		if path == "" {
			return fmt.Errorf("manifest has an empty executable path for %s", osArch)
		}
		if filepath.IsAbs(path) {
			return fmt.Errorf("executable path for %s must be relative, got %q", osArch, path)
		}
	}

	return nil
}

// checkBundle verifies that a staged bundle directory contains the manifest
// and every executable the manifest promises. Mattermost resolves those paths
// relative to the manifest, so a missing file means the plugin fails to start
// on that platform — a failure mode that would otherwise only surface on a
// live server.
func checkBundle(m *model.Manifest, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, manifestPath)); err != nil {
		return fmt.Errorf("bundle is missing %s: %w", manifestPath, err)
	}

	paths := make([]string, 0, len(m.Server.Executables)+1)
	for _, path := range m.Server.Executables {
		paths = append(paths, path)
	}
	if m.Server.Executable != "" {
		paths = append(paths, m.Server.Executable)
	}

	for _, path := range paths {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			return fmt.Errorf("bundle is missing executable %s: %w", path, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("bundle executable %s is empty", path)
		}
	}

	// The webapp bundle is checked by exactly the same rule, and for the same
	// reason. The server reads bundle_path when it serves the plugin to a
	// browser; a manifest that declares one and a bundle that does not carry
	// it produces a 404 at page load, which surfaces as "the record button is
	// not there" with nothing in the server log to explain it.
	if m.Webapp != nil && m.Webapp.BundlePath != "" {
		info, err := os.Stat(filepath.Join(dir, m.Webapp.BundlePath))
		if err != nil {
			return fmt.Errorf("bundle is missing the webapp bundle %s: %w",
				m.Webapp.BundlePath, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("webapp bundle %s is empty", m.Webapp.BundlePath)
		}
	}

	return nil
}

func run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%s", usage)
	}

	m, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}

	if err := validateManifest(m); err != nil {
		return err
	}

	switch args[0] {
	case "id":
		fmt.Println(m.Id)
	case "version":
		fmt.Println(m.Version)
	case "validate":
		fmt.Printf("plugin.json is valid: %s %s\n", m.Id, m.Version)
	case "check-bundle":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		if err := checkBundle(m, args[1]); err != nil {
			return err
		}
		fmt.Printf("bundle %s is complete for %s %s\n", args[1], m.Id, m.Version)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}

	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
