GO ?= go
DIST_DIR ?= build_assets
VERSION ?= dev
BUILD_TAGS := xray hysteria2 with_quic with_grpc with_utls with_wireguard with_acme with_gvisor
LDFLAGS := -X github.com/InazumaV/V2bX/cmd.version=$(VERSION) -s -w -buildid=

.PHONY: build-linux build-linux-amd64 build-linux-arm64

build-linux: build-linux-amd64 build-linux-arm64

build-linux-amd64:
	@mkdir -p $(DIST_DIR)
	GOEXPERIMENT=jsonv2 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build \
		-o $(DIST_DIR)/V2bX-linux-amd64 \
		-tags "$(BUILD_TAGS)" \
		-trimpath -ldflags "$(LDFLAGS)" .

build-linux-arm64:
	@mkdir -p $(DIST_DIR)
	GOEXPERIMENT=jsonv2 CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build \
		-o $(DIST_DIR)/V2bX-linux-arm64 \
		-tags "$(BUILD_TAGS)" \
		-trimpath -ldflags "$(LDFLAGS)" .
