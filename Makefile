export PATH := /usr/local/go/bin:$(PATH)
export GOPRIVATE := github.com/RakuenSoftware/*
export GONOSUMDB := github.com/RakuenSoftware/*
export GIT_CONFIG_COUNT := 1
export GIT_CONFIG_KEY_0 := url.git@github.com:.insteadOf
export GIT_CONFIG_VALUE_0 := https://github.com/

.PHONY: all build build-backend build-frontend build-backend-low build-frontend-low \
	build-low iso iso-low kernel kernel-low zfs zfs-low smoothkernel-low \
	test test-backend test-frontend lint lint-backend clean install deploy

all: build

# --- Build ---

build: build-backend build-frontend

VERSION ?= $(shell date -u +%Y.%m%d.%H%M)-$(shell git rev-parse --short HEAD)

build-backend:
	cd tierd && CGO_ENABLED=1 go build -ldflags "-X main.version=$(VERSION)" -o ../bin/tierd ./cmd/tierd/

build-frontend:
	cd tierd-ui && npm install --prefer-offline && npm run build

build-backend-low:
	./scripts/low-impact-build.sh $(MAKE) build-backend

build-frontend-low:
	./scripts/low-impact-build.sh $(MAKE) build-frontend

build-low: build-backend-low build-frontend-low

iso:
	./iso/build-iso.sh $(VERSION)

iso-low:
	./scripts/low-impact-build.sh ./iso/build-iso.sh $(VERSION)

smoothkernel-low:
	./scripts/build-smoothkernel.sh $(TARGETS)

kernel:
	./scripts/build-smoothkernel.sh kernel

kernel-low:
	./scripts/build-smoothkernel.sh kernel

zfs:
	./scripts/build-smoothkernel.sh zfs

zfs-low:
	./scripts/build-smoothkernel.sh zfs

# --- Lint ---

lint: lint-backend lint-frontend

lint-backend:
	cd tierd && CGO_ENABLED=1 go vet ./...

lint-frontend:
	cd tierd-ui && npm run lint

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
	install -m 644 tierd/deploy/tierd-host-init.service /etc/systemd/system/tierd-host-init.service
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
	rsync -av --rsync-path="sudo rsync" tierd/deploy/tierd-host-init.service $(HOST):/etc/systemd/system/tierd-host-init.service
	rsync -av --rsync-path="sudo rsync" tierd/deploy/tierd.service $(HOST):/etc/systemd/system/tierd.service
	ssh -t $(HOST) 'sudo systemctl daemon-reload && sudo systemctl restart tierd'

# --- Clean ---

clean:
	rm -rf bin/ tierd-ui/dist/
	cd tierd && go clean
