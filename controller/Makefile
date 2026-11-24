IMG ?= ghcr.io/example/notebook-route-controller:dev
PLATFORMS ?= linux/amd64,linux/arm64
ENVTEST_K8S_VERSION ?= 1.34.x
ENVTEST_VERSION ?= release-0.23
CHART_DIR ?= ../charts/kubeflow-notebooks-no-istio
# Upstream kubeflow/notebooks release whose images and CRD the chart pins.
KF_NOTEBOOKS_VERSION ?= v1.11.1

LOCALBIN ?= $(CURDIR)/bin
ENVTEST ?= $(LOCALBIN)/setup-envtest

.PHONY: all
all: fmt vet test build

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: ## Unit tests (no cluster required).
	go test ./internal/... -coverprofile coverage.out

.PHONY: test-integration
test-integration: $(ENVTEST) envtest-crds ## Integration tests against a real kube-apiserver with RBAC and the pinned CRDs.
	{ helm template it $(CHART_DIR) -n kubeflow -f $(CHART_DIR)/examples/dex.yaml; echo ---; \
	  helm template it $(CHART_DIR) -n kubeflow -f $(CHART_DIR)/examples/shared-gateway.yaml; } > $(LOCALBIN)/chart-render.yaml
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	ENVTEST_CRDS=$(LOCALBIN)/crds CHART_RENDER=$(LOCALBIN)/chart-render.yaml \
		go test ./tests/... -count=1 -v

.PHONY: envtest-crds
envtest-crds: ## Extract the real CRDs pinned by the chart (Gateway API, Envoy Gateway, Notebook).
	helm dependency build $(CHART_DIR) >/dev/null
	rm -rf $(LOCALBIN)/crds && mkdir -p $(LOCALBIN)/crds
	tar -xzf $(CHART_DIR)/charts/gateway-helm-*.tgz -C $(LOCALBIN)/crds --strip-components=4 \
		gateway-helm/charts/crds/crds/gatewayapi-crds.yaml
	tar -xzf $(CHART_DIR)/charts/gateway-helm-*.tgz -C $(LOCALBIN)/crds --strip-components=5 \
		gateway-helm/charts/crds/crds/generated
	cp $(CHART_DIR)/crds/*.yaml $(LOCALBIN)/crds/

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -o bin/manager ./cmd

.PHONY: run
run: ## Run against the current kubeconfig context.
	go run ./cmd --leader-elect=false

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-buildx
docker-buildx: ## Multi-arch build and push.
	docker buildx build --platform $(PLATFORMS) -t $(IMG) --push .

.PHONY: helm-lint
helm-lint: ## Lint and render the chart with every example values file.
	helm lint $(CHART_DIR) -f $(CHART_DIR)/examples/dex.yaml
	@for f in $(CHART_DIR)/examples/*.yaml; do \
		echo "--- $$f"; helm template test $(CHART_DIR) -n kubeflow -f $$f > /dev/null || exit 1; \
	done

.PHONY: sync-notebook-crd
sync-notebook-crd: ## Re-render the upstream Notebook CRD (unmodified) into the chart.
	{ printf '# notebooks.kubeflow.org CRD rendered unmodified from upstream:\n#   repo: https://github.com/kubeflow/notebooks  tag: $(KF_NOTEBOOKS_VERSION)\n#   kubectl kustomize components/notebook-controller/config/crd\n# Regenerate with: make -C controller sync-notebook-crd\n'; \
	  kubectl kustomize "https://github.com/kubeflow/notebooks//components/notebook-controller/config/crd?ref=$(KF_NOTEBOOKS_VERSION)"; \
	} > $(CHART_DIR)/crds/notebooks.kubeflow.org.yaml

$(ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
