.PHONY: build run clean deps install

BINARY_NAME=$(shell basename $(CURDIR))

build: deps
	go build -o $(BINARY_NAME) main.go

run: build
	./$(BINARY_NAME)

clean:
	go clean
	rm -f $(BINARY_NAME)

deps:
	go get ./...
	go mod tidy

install: build
	sudo cp $(BINARY_NAME) /usr/local/bin/