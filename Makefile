export PATH := /usr/local/go/bin:$(PATH)

.PHONY: all build build-backend build-frontend build-fuse-ns test test-backend test-frontend lint lint-backend clean install deploy

all: build

# --- Build ---

build: build-backend build-frontend build-fuse-ns

VERSION ?= $(shell date -u +%Y.%m%d.%H%M)-$(shell git rev-parse --short HEAD)

build-backend:
	cd tierd && CGO_ENABLED=1 go build -ldflags "-X main.version=$(VERSION)" -o ../bin/tierd ./cmd/tierd/

build-frontend:
	cd tierd-ui && npm install --prefer-offline && npm run build

build-fuse-ns:
	@if pkg-config --exists fuse3 2>/dev/null; then \
		$(MAKE) -C src/fuse-ns; \
	else \
		echo "WARNING: libfuse3-dev not found; skipping tierd-fuse-ns build"; \
	fi

# --- Lint ---

lint: lint-backend

lint-backend:
	cd tierd && CGO_ENABLED=1 go vet ./...

# --- Test ---

test: test-backend test-frontend

test-backend:
	cd tierd && CGO_ENABLED=1 go test ./... -count=1

test-frontend:
	cd tierd-ui && npm test

# --- Install (for deployment) ---

install: build
	install -m 755 bin/tierd /usr/local/bin/tierd
	mkdir -p /usr/share/tierd-ui
	cp -r tierd-ui/dist/smoothnas-ui/* /usr/share/tierd-ui/
	install -m 644 tierd/deploy/tierd.service /etc/systemd/system/tierd.service
	install -m 644 tierd/deploy/nginx.conf /etc/nginx/sites-available/tierd
	ln -sf /etc/nginx/sites-available/tierd /etc/nginx/sites-enabled/tierd
	rm -f /etc/nginx/sites-enabled/default
	bash tierd/deploy/generate-tls.sh
	systemctl daemon-reload
	systemctl enable tierd.service
	systemctl enable nginx.service

# --- Remote deploy (dev shortcut) ---
# Usage: make deploy HOST=root@192.168.1.x

HOST ?= root@smoothnas

deploy: build
	rsync -av --delete --rsync-path="sudo rsync" tierd-ui/dist/smoothnas-ui/ $(HOST):/usr/share/tierd-ui/
	rsync -av --rsync-path="sudo rsync" bin/tierd $(HOST):/usr/local/bin/tierd
	ssh -t $(HOST) 'sudo systemctl restart tierd'

# --- Clean ---

clean:
	rm -rf bin/ tierd-ui/dist/
	cd tierd && go clean
