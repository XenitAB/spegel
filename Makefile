TAG = $$(git rev-parse --short HEAD)
IMG ?= ghcr.io/xenitab/spegel:$(TAG)

all: lint


lint:
	golangci-lint run ./...

test:
	go test ./...

docker-build:
	docker build -t ${IMG} .

.PHONY: e2e
.ONESHELL:
e2e: docker-build
	set -ex

	# Create Kind cluster
	TMP_DIR=$$(mktemp -d)
	export KIND_KUBECONFIG=$$TMP_DIR/kind.kubeconfig
	echo $$KIND_KUBECONFIG
	kind create cluster --kubeconfig $$KIND_KUBECONFIG --config ./e2e/kind-config.yaml

	# Pull images onto single node which will never run workload.
	docker exec kind-worker ctr -n k8s.io image pull docker.io/library/nginx:1.23.0
	docker exec kind-worker ctr -n k8s.io image pull docker.io/library/nginx@sha256:b3a676a9145dc005062d5e79b92d90574fb3bf2396f4913dc1732f9065f55c4b

	# Deploy Spegel
	kind load docker-image ${IMG}
	kubectl --kubeconfig $$KIND_KUBECONFIG create namespace spegel
	helm --kubeconfig $$KIND_KUBECONFIG upgrade --install --namespace="spegel" spegel ./charts/spegel --set "image.pullPolicy=Never" --set "image.tag=${TAG}"
	kubectl --kubeconfig $$KIND_KUBECONFIG --namespace spegel rollout status daemonset spegel --timeout 60s

	# Pull images onto single node which will never run workload.
	docker exec kind-worker ctr -n k8s.io image pull docker.io/library/nginx:1.21.0@sha256:2f1cd90e00fe2c991e18272bb35d6a8258eeb27785d121aa4cc1ae4235167cfd

	# Remove default route to disable internet access
	docker exec kind-control-plane ip route del default
	docker exec kind-worker ip route del default
	docker exec kind-worker2 ip route del default
	docker exec kind-worker3 ip route del default

	# Deploy test Nginx pods and expect pull to work
	kubectl --kubeconfig $$KIND_KUBECONFIG apply -f ./e2e/test-nginx.yaml
	kubectl --kubeconfig $$KIND_KUBECONFIG --namespace nginx wait deployment/nginx-tag --for condition=available
	kubectl --kubeconfig $$KIND_KUBECONFIG --namespace nginx wait deployment/nginx-digest --for condition=available
	kubectl --kubeconfig $$KIND_KUBECONFIG --namespace nginx wait deployment/nginx-tag-and-digest --for condition=available

	# Delete cluster
	kind delete cluster

helm-docs: tools
	cd ./charts/spegel && helm-docs

tools:
	GO111MODULE=on go install github.com/norwoodj/helm-docs/cmd/helm-docs