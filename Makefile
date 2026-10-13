BINARY = morctl
DIST   = dist

.PHONY: all build test vet check build-all clean

all: build

build:
	go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

check: vet test

# Cross-compile the four supported targets. Pure stdlib, so no cgo and no
# per-platform toolchain is needed.
build-all: clean
	@mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 go build -o $(DIST)/$(BINARY)-windows-amd64.exe .
	GOOS=linux   GOARCH=amd64 go build -o $(DIST)/$(BINARY)-linux-amd64 .
	GOOS=darwin  GOARCH=amd64 go build -o $(DIST)/$(BINARY)-darwin-amd64 .
	GOOS=darwin  GOARCH=arm64 go build -o $(DIST)/$(BINARY)-darwin-arm64 .
	@echo "built:" && ls -1 $(DIST)

clean:
	rm -rf $(DIST) $(BINARY) $(BINARY).exe
