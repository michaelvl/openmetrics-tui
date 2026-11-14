BINARY_NAME=omtui
MOCK_BINARY_NAME=mock-server
DASH_BINARY_NAME=omtui-dash

.PHONY: all build dash test lint fmt clean run mock-server screenshot

all: build dash mock-server

build:
	CGO_ENABLED=0 go build -o $(BINARY_NAME) ./cmd/omtui

dash:
	CGO_ENABLED=0 go build -o $(DASH_BINARY_NAME) ./cmd/omtui-dash

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
	rm -f $(BINARY_NAME) $(DASH_BINARY_NAME) $(MOCK_BINARY_NAME)

run: build
	./$(BINARY_NAME)
