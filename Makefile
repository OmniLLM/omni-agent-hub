.PHONY: build run clean test fmt lint logs install \
       start stop restart status

BINARY := oah
CMD := ./cmd/omni-agent-hub
SERVICE := omni-agent-hub.service
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/OmniLLM/omni-agent-hub/internal/cli.version=$(VERSION)"

build:
	go build $(LDFLAGS) -o $(BINARY) $(CMD)

run: build
	./$(BINARY) serve

run-dev:
	go run $(CMD) serve

install: build
	cp $(BINARY) $(HOME)/go/bin/$(BINARY)
	# Keep legacy name as a symlink for backward compatibility.
	ln -sf $(BINARY) $(HOME)/go/bin/omni-agent-hub
	@echo "Installed to $(HOME)/go/bin/$(BINARY)"

# ── daemon management (PID-file based) ─────────────────────────

start: build
	./$(BINARY) start

stop:
	./$(BINARY) stop

restart: build
	./$(BINARY) restart

status:
	./$(BINARY) status

logs:
	./$(BINARY) logs

# ── upstream management ────────────────────────────────────────

upstream-list:
	./$(BINARY) upstream list

upstream-refresh:
	./$(BINARY) upstream refresh

# ── systemd service (optional) ─────────────────────────────────

install-service: install
	sudo cp $(SERVICE) /etc/systemd/system/
	sudo systemctl daemon-reload
	@echo "Service installed. Run 'sudo systemctl enable --now $(SERVICE)' to activate."

uninstall-service:
	sudo systemctl disable --now $(SERVICE) 2>/dev/null || true
	sudo rm -f /etc/systemd/system/$(SERVICE)
	sudo systemctl daemon-reload

# ── development ────────────────────────────────────────────────

test:
	go test ./...

fmt:
	go fmt ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
