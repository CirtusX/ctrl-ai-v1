BINARY  := ctrlai
PKG     := github.com/ctrlai/ctrlai
CMD     := ./cmd/ctrlai

.PHONY: build test lint clean install

build:
	go build -o $(BINARY) $(CMD)

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -f $(BINARY)

install:
	go install $(CMD)
