BINARY_NAME=omtui
MOCK_BINARY_NAME=mock-server
MOCK_PORT=8080
DASH_BINARY_NAME=omtui-dash
KIND_CLUSTER=omtui

.PHONY: all build tui dash test lint fmt clean run run-dash check-example mock-server screenshot kind-up kind-down kind-dash

all: build mock-server

build: tui dash

tui:
	CGO_ENABLED=0 go build -o $(BINARY_NAME) ./cmd/omtui

dash:
	CGO_ENABLED=0 go build -o $(DASH_BINARY_NAME) ./cmd/omtui-dash

mock-server:
	CGO_ENABLED=0 go build -o $(MOCK_BINARY_NAME) ./cmd/mock-server

test:
	go test -v ./...

lint:
	golangci-lint run

fmt:
	go fmt ./...

clean:
	go clean
	rm -f $(BINARY_NAME) $(DASH_BINARY_NAME) $(MOCK_BINARY_NAME)

run: tui
	./$(BINARY_NAME)

# Run the dashboard against the mock server's Prometheus API. The mock is
# stopped again when the dashboard exits.
run-dash: dash mock-server
	./$(MOCK_BINARY_NAME) -port $(MOCK_PORT) >/dev/null & mock=$$!; \
	trap 'kill $$mock' EXIT; \
	sleep 1; \
	./$(DASH_BINARY_NAME) --prom http://localhost:$(MOCK_PORT) examples/controllers.yaml

# Validate the example dashboard and run every panel query against the mock.
check-example: dash mock-server
	./$(MOCK_BINARY_NAME) -port $(MOCK_PORT) >/dev/null & mock=$$!; \
	trap 'kill $$mock' EXIT; \
	sleep 1; \
	./$(DASH_BINARY_NAME) --check --prom http://localhost:$(MOCK_PORT) examples/controllers.yaml

# Re-record docs/dashboard.png (needs vhs).
screenshot: dash mock-server
	./$(MOCK_BINARY_NAME) -port $(MOCK_PORT) >/dev/null & mock=$$!; \
	trap 'kill $$mock' EXIT; \
	sleep 1; \
	vhs docs/dashboard.tape

# Local test cluster with demo workloads (needs kind, docker or podman, and kubectl).
kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)
	kubectl --context kind-$(KIND_CLUSTER) apply -f examples/kind/workloads.yaml
	kubectl --context kind-$(KIND_CLUSTER) -n demo wait --for=condition=Available deployment --all --timeout=180s

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

# Run the dashboard in managed mode against the kind cluster.
kind-dash: dash
	./$(DASH_BINARY_NAME) --managed --kube-context kind-$(KIND_CLUSTER) examples/kind-dashboard.yaml
