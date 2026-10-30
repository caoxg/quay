# Quay — build, test and package.
#   make            native GUI + CLI binary in dist/
#   make app        macOS Quay.app for this Mac (GUI; also the CLI: Quay.app/Contents/MacOS/quay status)
#   make cli        CLI-only binary (no cgo, cross-compiles anywhere)
#   make mac        universal Quay.app as .dmg and .zip                       (needs macOS)
#   make windows    Quay.exe (GUI) + bin\quay.exe (CLI) zips, amd64 and arm64  (any OS)
#   make linux      GUI .tar.gz and .deb, amd64 and arm64                      (needs Docker)
#   make release    all of the above + CLI binaries for every platform + SHA256SUMS
#   make licenses   regenerate internal/legal/THIRD_PARTY_LICENSES.txt after changing dependencies
#   make test       unit tests          make e2e   end-to-end tests on a Docker fake board
#   make profiles   take ../quay-profiles as the built-in copy of the profiles repository
#   make preview    the UI with a mock backend at http://127.0.0.1:8765/?mock (&os=windows|linux)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Windows version resources and Info.plist want a plain x.y.z.
NUMVER  := $(shell echo '$(VERSION)' | grep -oE '^v?[0-9]+(\.[0-9]+){0,2}' | sed 's/^v//')
NUMVER  := $(or $(NUMVER),0.0.0)
LDFLAGS := -s -w -X github.com/caoxg/quay/internal/version.Version=$(VERSION)
GOARCH  ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
GOOS    := $(shell go env GOOS)
EXE     := dist/quay-$(GOOS)-$(GOARCH)
STAGE   := dist/.stage
# Shipped next to every binary (and embedded in it: `quay licenses`).
LEGAL   := LICENSE NOTICE internal/legal/THIRD_PARTY_LICENSES.txt
WINRES  := go run github.com/tc-hib/go-winres@v0.3.3
LINUX_ARCHS ?= amd64 arm64
WINDOWS_ARCHS ?= amd64 arm64

# The GUI links the system web view: WebKit through cgo on macOS and Linux, WebView2 through plain
# syscalls on Windows (so Windows cross-compiles with CGO_ENABLED=0).
GUI_TAGS := production
ifeq ($(GOOS),darwin)
  export CGO_CFLAGS  = -mmacosx-version-min=12.0
  export CGO_LDFLAGS = -mmacosx-version-min=12.0
endif
# Linux: GTK 3 + WebKitGTK 4.1 are available on more distributions than GTK 4 + WebKitGTK 6.
ifeq ($(GOOS),linux)
  GUI_TAGS += gtk3
  TEST_TAGS := -tags gtk3
endif

# $(call bundle,BINARY) wraps a macOS binary into dist/Quay.app and signs it ad hoc.
define bundle
	rm -rf dist/Quay.app
	mkdir -p dist/Quay.app/Contents/MacOS dist/Quay.app/Contents/Resources
	cp $(1) dist/Quay.app/Contents/MacOS/quay
	cp build/icon/icon.icns dist/Quay.app/Contents/Resources/icon.icns
	cp $(LEGAL) dist/Quay.app/Contents/Resources/
	sed 's/NUMVER/$(NUMVER)/g' build/macos/Info.plist > dist/Quay.app/Contents/Info.plist
	codesign --force --deep --sign - dist/Quay.app
endef

.PHONY: build cli app mac windows linux clis checksums licenses release profiles test e2e preview screenshots clean

build:
	GOARCH=$(GOARCH) CGO_ENABLED=1 go build -tags "$(GUI_TAGS)" -trimpath -ldflags "$(LDFLAGS)" -o $(EXE) .
	@echo "built $(EXE)"

cli:
	CGO_ENABLED=0 go build -tags nogui -trimpath -ldflags "$(LDFLAGS)" -o dist/quay-cli-$(GOOS)-$(GOARCH) .
	@echo "built dist/quay-cli-$(GOOS)-$(GOARCH)"

app: build
	$(call bundle,$(EXE))
	@echo "built dist/Quay.app"

mac:
	mkdir -p $(STAGE)/mac
	GOARCH=arm64 CGO_ENABLED=1 go build -tags "$(GUI_TAGS)" -trimpath -ldflags "$(LDFLAGS)" -o $(STAGE)/mac/quay-arm64 .
	GOARCH=amd64 CGO_ENABLED=1 go build -tags "$(GUI_TAGS)" -trimpath -ldflags "$(LDFLAGS)" -o $(STAGE)/mac/quay-amd64 .
	lipo -create -output $(STAGE)/mac/quay $(STAGE)/mac/quay-arm64 $(STAGE)/mac/quay-amd64
	$(call bundle,$(STAGE)/mac/quay)
	rm -f dist/Quay-$(VERSION)-macos-universal.zip dist/Quay-$(VERSION)-macos-universal.dmg
	ditto -c -k --keepParent dist/Quay.app dist/Quay-$(VERSION)-macos-universal.zip
	rm -rf $(STAGE)/dmg && mkdir -p $(STAGE)/dmg && cp -R dist/Quay.app $(STAGE)/dmg/ && ln -s /Applications $(STAGE)/dmg/Applications
	hdiutil create -quiet -volname Quay -srcfolder $(STAGE)/dmg -fs HFS+ -format UDZO -ov dist/Quay-$(VERSION)-macos-universal.dmg
	@echo "built dist/Quay-$(VERSION)-macos-universal.{dmg,zip}"

# Each zip holds Quay\Quay.exe (double-click: the app) and Quay\bin\quay.exe (the CLI; a GUI
# program cannot also be a well-behaved console program on Windows).
windows:
	$(WINRES) make --in build/windows/winres.json --arch $(shell echo $(WINDOWS_ARCHS) | tr ' ' ,) --out rsrc \
	  --product-version $(NUMVER) --file-version $(NUMVER)
	@set -e; trap 'rm -f rsrc_windows_*.syso' EXIT; for a in $(WINDOWS_ARCHS); do \
	  d=$(STAGE)/windows-$$a/Quay; rm -rf $(STAGE)/windows-$$a; mkdir -p $$d/bin; \
	  echo "→ windows/$$a"; \
	  GOOS=windows GOARCH=$$a CGO_ENABLED=0 go build -tags "production" -trimpath -ldflags "$(LDFLAGS) -H windowsgui" -o $$d/Quay.exe .; \
	  GOOS=windows GOARCH=$$a CGO_ENABLED=0 go build -tags nogui -trimpath -ldflags "$(LDFLAGS)" -o $$d/bin/quay.exe .; \
	  cp README.md $(LEGAL) $$d/; \
	  rm -f dist/Quay-$(VERSION)-windows-$$a.zip; (cd $(STAGE)/windows-$$a && zip -qr ../../Quay-$(VERSION)-windows-$$a.zip Quay); \
	  echo "built dist/Quay-$(VERSION)-windows-$$a.zip"; \
	done

# Built in Debian 12 containers (GTK 3, WebKitGTK 4.1); the non-native architecture runs emulated.
linux:
	@set -e; for a in $(LINUX_ARCHS); do \
	  echo "→ linux/$$a"; \
	  docker build -q --platform linux/$$a -t quay-linux-build:$$a build/linux >/dev/null; \
	  docker run --rm --platform linux/$$a -v "$(CURDIR)":/src -v "$$(go env GOMODCACHE)":/go/pkg/mod \
	    -v quay-gocache-$$a:/root/.cache/go-build -e VERSION=$(VERSION) -e HOST_UID=$$(id -u) -e HOST_GID=$$(id -g) \
	    quay-linux-build:$$a bash build/linux/package.sh; \
	done

clis:
	@set -e; for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  echo "→ cli $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -tags nogui -trimpath -ldflags "$(LDFLAGS)" -o dist/quay-cli-$(VERSION)-$$os-$$arch$$ext .; \
	done

licenses:
	bash build/licenses.sh

# SHA256SUMS, signed with the release key (build/sign: $$QUAY_SIGNING_KEY or
# ~/.config/quay/release-signing.key). Quay's self-update installs nothing unsigned.
checksums:
	cd dist && rm -f SHA256SUMS SHA256SUMS.sig && shasum -a 256 $$(ls | grep -E '\.(zip|dmg|tar\.gz|deb)$$|^quay-cli-') > SHA256SUMS
	go run ./build/sign sign dist/SHA256SUMS
	@echo "wrote dist/SHA256SUMS and its signature"

# The notes every release ends with (the macOS first-launch steps, until Quay is signed):
#   gh release create vX.Y.Z dist/* --notes-file <(cat my-notes.md build/release-notes-footer.md)

ifeq ($(GOOS),darwin)
release: clean licenses mac windows linux clis checksums
else
release: clean licenses windows linux clis checksums
endif
	rm -rf $(STAGE)
	@ls -1 dist

# Take the profiles repository (PROFILES, default ../quay-profiles) as Quay's built-in copy.
PROFILES ?= ../quay-profiles
profiles:
	@test -f $(PROFILES)/index.yaml || { echo "no $(PROFILES)/index.yaml (PROFILES=dir)"; exit 1; }
	rm -rf internal/hub/builtin && mkdir -p internal/hub/builtin
	cp -R $(PROFILES)/index.yaml $(PROFILES)/apps $(PROFILES)/systems internal/hub/builtin/
	go test ./internal/hub ./internal/profile ./internal/sysprofile

test:
	go vet $(TEST_TAGS) ./...
	go test $(TEST_TAGS) ./internal/...
	@if command -v node >/dev/null; then \
	  for f in internal/gui/frontend/*.js; do node --input-type=module --check < $$f || { echo "syntax error in $$f"; exit 1; }; done; \
	  echo "frontend: syntax ok"; \
	else echo "frontend: node not found, skipped the syntax check"; fi

e2e:
	go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/

preview:
	python3 -m http.server 8765 --bind 127.0.0.1 -d internal/gui/frontend

# README screenshots from the mock backend (needs uv; uses Playwright's headless Chromium).
screenshots:
	uv run build/screenshots.py

clean:
	rm -rf dist rsrc_windows_*.syso
