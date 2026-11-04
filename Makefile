# Meridian Trellis build.
#
#   make            build bin/trellis for this machine (plus meshd/meshctl links)
#   make cross      build dist/ binaries for arm64, armv7 and amd64
#   make test       unit tests (no root)
#   make lab        lab tests in network namespaces (root, or a user namespace)
#   make check      gofmt, vet and tests
#   make gen        regenerate netlink constants from the kernel headers
#   make deploy     check, build and update every node in deploy.hosts
#   make lab-scale  scale lab: 50 nodes, ID convergence and gossip volume (~6 min)
#   make nodes      show meshctl status from every node in deploy.hosts
#   make band BAND=2g [CHANNEL=6]   switch mesh-profile.yaml to a band and push it to every node
#   make profile    push mesh-profile.yaml to every node and apply it live

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
LDFLAGS  = -s -w -X main.version=$(VERSION)
GOFLAGS  = -trimpath
PKG      = ./cmd/trellis
ARCHES   = arm64 arm armv6 amd64

.PHONY: all build cross test lab lab-scale check fmt vet gen clean deploy nodes band profile

all: build

build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/trellis $(PKG)
	ln -sf trellis bin/meshd
	ln -sf trellis bin/meshctl

cross:
	@mkdir -p dist
	@for arch in $(ARCHES); do \
	  out=dist/trellis-linux-$$arch; \
	  goarch=$$arch; \
	  if [ $$arch = arm ]; then export GOARM=7; out=dist/trellis-linux-armv7; fi; \
	  if [ $$arch = armv6 ]; then export GOARM=6; goarch=arm; fi; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$goarch $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done
	@ls -la dist/

test:
	$(GO) test ./...

lab: build
	./lab/lab.sh

lab-scale: build
	./lab/lab.sh --scale

fmt:
	@test -z "$$(gofmt -l cmd internal lab tools 2>/dev/null)" || { gofmt -l cmd internal lab tools; echo "run gofmt -w"; exit 1; }

vet:
	$(GO) vet ./...

check: fmt vet test

gen:
	python3 -I tools/genconst/genconst.py --header /usr/include/linux/batman_adv.h --package batadv > internal/batadv/consts_gen.go
	python3 -I tools/genconst/genconst.py --header /usr/include/linux/nl80211.h --package nl80211 \
	  --enum nl80211_commands --enum nl80211_attrs --enum nl80211_iftype --enum nl80211_band_attr \
	  --enum nl80211_frequency_attr --enum nl80211_band --enum nl80211_iface_limit_attrs \
	  --enum nl80211_if_combination_attrs --enum nl80211_chan_width --enum nl80211_channel_type \
	  --enum nl80211_tx_power_setting --enum nl80211_reg_initiator --enum nl80211_reg_type \
	  --enum nl80211_reg_rule_attr --enum nl80211_reg_rule_flags --enum nl80211_dfs_regions \
	  --enum nl80211_meshconf_params --enum nl80211_mesh_setup_params --enum nl80211_sta_info \
	  --enum nl80211_rate_info --enum nl80211_mpath_info --enum nl80211_feature_flags \
	  --enum nl80211_ext_feature_index --enum nl80211_band_iftype_attr --enum nl80211_user_reg_hint_type \
	  --enum nl80211_mntr_flags --enum nl80211_plink_state --enum nl80211_mesh_power_mode \
	  > internal/nl80211/consts_gen.go
	gofmt -w internal/batadv/consts_gen.go internal/nl80211/consts_gen.go

clean:
	rm -rf bin dist

deploy:
	scripts/deploy.sh $(HOSTS)

nodes:
	scripts/deploy.sh --status $(HOSTS)

band: build
	@test -n "$(BAND)" || { echo "usage: make band BAND=2g|5g|6g [CHANNEL=N] [HOSTS=...]"; exit 2; }
	bin/meshctl profile band $(BAND) $(if $(CHANNEL),--channel $(CHANNEL)) mesh-profile.yaml
	scripts/deploy.sh --profile $(HOSTS)

profile:
	scripts/deploy.sh --profile $(HOSTS)
