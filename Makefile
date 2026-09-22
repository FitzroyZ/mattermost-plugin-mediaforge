GO ?= go

# Platforms the server binary is cross-compiled for. Keep in sync with the
# executables map in plugin.json; `make dist` verifies that every declared
# executable actually ended up in the bundle.
PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64

# Identity is read from plugin.json inside the recipes rather than via a
# top-level $(shell ...): Make ignores the exit status of $(shell ...), so a
# broken manifest would silently expand to an empty string, and a top-level
# assignment would run the helper on every invocation including `make clean`.

## all: check-style, test, then dist.
.PHONY: all
all: check-style test dist

## check-style: go vet and go build over the whole module.
.PHONY: check-style
check-style:
	$(GO) vet ./...
	$(GO) build ./...

## test: the Go suite and the webapp tests.
.PHONY: test
test: webapp-test
	$(GO) test ./... -count=1

## webapp-test: runs the record button's contract tests.
#
# Node's own test runner, so there is nothing to install: the webapp part is
# one dependency-free file and its tests are plain `node --test`.
#
# The glob is expanded by the SHELL, not by node, and that is deliberate.
# Quoting it (`node --test 'tests/*.test.js'`) hands the pattern to node, which
# only learned to expand one in v22 — on v20 the target dies with "Could not
# find '.../tests/*.test.js'", which reads like the test files are missing
# rather than like the runtime is too old, and sends the reader looking in
# entirely the wrong place. Passing the directory instead has the mirror
# problem: v20 walks it, v22 treats it as a single test and fails. The shell
# glob is the one form both versions agree on.
#
# Only the first line carries `##`, because `make help` lists those and a
# thirteen-line rationale is not a target description.
.PHONY: webapp-test
webapp-test:
	cd webapp && node --test tests/*.test.js

## webapp: places the webapp bundle where plugin.json says it is.
#
# There is no bundler because there is nothing to bundle: the source has no
# imports, and React is taken from the window the way Mattermost publishes it
# for plugins. The copy exists so the built bundle is a build ARTEFACT rather
# than the source file itself — `make clean` must be able to remove it, and
# the bundle must not change when someone edits the source without rebuilding.
.PHONY: webapp
webapp:
	rm -rf webapp/dist
	mkdir -p webapp/dist
	cp webapp/src/mediaforge.js webapp/dist/main.js

## manifest: validates plugin.json against the Mattermost manifest rules.
.PHONY: manifest
manifest:
	$(GO) run ./build/manifest validate

## server: cross-compiles the server binary for every supported platform.
.PHONY: server
server:
	rm -rf server/dist
	mkdir -p server/dist
	@set -e; for platform in $(PLATFORMS); do \
		goos=$${platform%-*}; \
		goarch=$${platform#*-}; \
		output=server/dist/plugin-$$platform; \
		if [ "$$goos" = "windows" ]; then output=$$output.exe; fi; \
		echo "building $$output"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch \
			$(GO) build -trimpath -ldflags="-s -w" -o $$output ./server; \
	done

## dist: builds the installable plugin bundle.
.PHONY: dist
dist: manifest server webapp
	@set -e; \
	id=$$($(GO) run ./build/manifest id); \
	version=$$($(GO) run ./build/manifest version); \
	if [ -z "$$id" ] || [ -z "$$version" ]; then \
		echo "cannot read plugin id or version from plugin.json" >&2; \
		exit 1; \
	fi; \
	bundle=mattermost-plugin-mediaforge-$$version.tar.gz; \
	rm -rf dist; \
	mkdir -p "dist/$$id/server" "dist/$$id/webapp"; \
	cp plugin.json "dist/$$id/"; \
	cp -r server/dist "dist/$$id/server/dist"; \
	cp -r webapp/dist "dist/$$id/webapp/dist"; \
	$(GO) run ./build/manifest check-bundle "dist/$$id"; \
	tar -C dist -czf "dist/$$bundle" "$$id"; \
	echo "bundle: dist/$$bundle"

## clean: removes all build output.
.PHONY: clean
clean:
	rm -rf dist server/dist webapp/dist

## help: lists the documented targets.
.PHONY: help
help:
	@grep -E '^## ' Makefile | sed 's/^## //'
