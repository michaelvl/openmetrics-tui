BINARY_NAME=omtui
MOCK_BINARY_NAME=mock-server
MOCK_PORT=8080
DASH_BINARY_NAME=omtui-dash

.PHONY: all build dash test lint fmt clean run run-dash check-example demo mock-server screenshot

all: build dash mock-server

build:
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

run: build
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

# Re-record docs/dashboard.gif and docs/dashboard.png (needs vhs).
demo: dash mock-server
	./$(MOCK_BINARY_NAME) -port $(MOCK_PORT) >/dev/null & mock=$$!; \
	trap 'kill $$mock' EXIT; \
	sleep 1; \
	vhs docs/dashboard.tape
