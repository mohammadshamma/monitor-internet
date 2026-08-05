BIN      := mon
PREFIX   := $(HOME)/.local/bin
TARGET   := $(PREFIX)/$(BIN)
PKG      := ./cmd/mon

# CGO_ENABLED=0 is the point of choosing Go here: the resulting binary carries
# every dependency inside it and links nothing but libSystem, so no package
# manager can break the agent once it is built.
export CGO_ENABLED := 0

.PHONY: all build test vet fmt install uninstall run dashboard vendor clean check

all: check build

build:
	go build -trimpath -o $(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w internal cmd

check: vet test

# Install atomically: write to a temp file in the destination directory, then
# rename. A rebuild can never leave launchd holding a half-written executable.
install: build
	@mkdir -p $(PREFIX)
	@cp $(BIN) $(TARGET).tmp
	@mv -f $(TARGET).tmp $(TARGET)
	@echo "installed $(TARGET)"
	@if $(TARGET) restart-agent >/dev/null 2>&1; then \
		echo "restarted both launchd agents"; \
	else \
		echo "agents not loaded — run: $(TARGET) install-agent"; \
	fi

uninstall:
	-$(TARGET) uninstall-agent
	rm -f $(TARGET)

# Commit dependencies into the repo so even a rebuild needs no network and no
# populated module cache.
vendor:
	go mod tidy
	go mod vendor

run: build
	./$(BIN) start

dashboard: build
	./$(BIN) serve --host 127.0.0.1

clean:
	rm -f $(BIN)
