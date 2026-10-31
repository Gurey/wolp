VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD ?=
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.build=$(BUILD) -X main.commit=$(COMMIT)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

# os/arch[/arm version]. The agent only does real work on Linux, so other
# systems get just the proxy.
PLATFORMS := linux/amd64 linux/arm64 linux/arm/7 linux/arm/6 darwin/arm64 darwin/amd64

.PHONY: build test dist clean

build:
	$(GOBUILD) -o wolp ./cmd/wolp
	$(GOBUILD) -o wolpa ./cmd/wolpa

test:
	go vet ./...
	go test -race ./...

# dist builds one archive per platform plus SHA256SUMS into dist/.
dist: test
	rm -rf dist
	@set -e; for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); arch=$$(echo $$p | cut -d/ -f2); arm=$$(echo $$p | cut -d/ -f3); \
		suffix=$$os-$$arch$${arm:+v$$arm}; \
		dir=dist/wolp-$(VERSION)-$$suffix; \
		mkdir -p $$dir; \
		echo "building $$suffix"; \
		GOOS=$$os GOARCH=$$arch GOARM=$$arm $(GOBUILD) -o $$dir/wolp ./cmd/wolp; \
		cp wolp.toml README.md $$dir/; \
		if [ $$os = linux ]; then \
			GOOS=$$os GOARCH=$$arch GOARM=$$arm $(GOBUILD) -o $$dir/wolpa ./cmd/wolpa; \
			cp wolpa.toml deploy/wolp.service deploy/wolpa.service $$dir/; \
		fi; \
		tar -czf $$dir.tar.gz -C dist $$(basename $$dir); \
		rm -rf $$dir; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS 2>/dev/null || shasum -a 256 *.tar.gz > SHA256SUMS
	@ls -lh dist

clean:
	rm -rf dist wolp wolpa
