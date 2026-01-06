BINARY_NAME=openmetrics-tui
MOCK_BINARY_NAME=mock-server

.PHONY: all build test lint fmt clean run mock-server

all: build mock-server

build:
	CGO_ENABLED=0 go build -o $(BINARY_NAME) .

mock-server:
	CGO_ENABLED=0 go build -o $(MOCK_BINARY_NAME) cmd/mock-server/main.go

test:
	go test -v ./...

lint:
	golangci-lint run

fmt:
	go fmt ./...

clean:
	go clean
	rm -f $(BINARY_NAME) $(MOCK_BINARY_NAME)

run: build
	./$(BINARY_NAME)
