SHELL := /bin/sh

# Windows needs the .exe suffix for the binary to be runnable from PowerShell.
EXE   :=
ifeq ($(OS),Windows_NT)
EXE   := .exe
endif

BIN   := bin/truegrain$(EXE)
MODEL := testdata/models
WS    := testdata/workspace
DB    := build/demo.duckdb

# bin/ is searched first so a duckdb binary dropped there is picked up without
# a system install. See `make duckdb`.
export PATH := $(CURDIR)/bin:$(PATH)

.PHONY: help build test test-all lint golden demo teams duckdb validate compile query health mcp rest console console-dev clean

help:
	@echo "build      compile the truegrain binary into $(BIN)"
	@echo "test       run every test that needs no database"
	@echo "test-all   run everything including the parity suite (needs duckdb on PATH)"
	@echo "golden     regenerate the committed SQL snapshots"
	@echo "duckdb     download the DuckDB CLI into bin/ (single executable, no installer)"
	@echo "demo       seed $(DB) and run one query end to end"
	@echo "teams      the two-team workspace: namespaces, imports and governance across them"
	@echo "validate   check the fixture model"
	@echo "mcp        serve the fixture model to an agent over stdio"
	@echo "clean      remove build output"

build:
	@mkdir -p bin
	go build -o $(BIN) ./cmd/truegrain

test:
	go test ./...

test-all:
	go test -count=1 ./...
	@command -v duckdb >/dev/null 2>&1 || { \
		echo; \
		echo "duckdb is not on PATH, so the parity suite was skipped."; \
		echo "It is a single executable: https://duckdb.org/docs/installation/"; \
		exit 1; }

# The DuckDB CLI is one self-contained executable. It goes in bin/, which the
# PATH above already covers, so nothing is installed system wide.
duckdb:
	@mkdir -p bin
	@case "$$(uname -s)" in 	  MINGW*|MSYS*|CYGWIN*) f=duckdb_cli-windows-amd64.zip ;; 	  Darwin)               f=duckdb_cli-osx-universal.zip ;; 	  *)                    f=duckdb_cli-linux-amd64.zip ;; 	esac; 	echo "downloading $$f"; 	curl -fsSL -o bin/duckdb.zip "https://github.com/duckdb/duckdb/releases/latest/download/$$f"
	cd bin && unzip -o duckdb.zip && rm duckdb.zip
	@bin/duckdb --version

lint:
	gofmt -l ./cmd ./internal
	go vet ./...

golden:
	go test ./internal/dialect/ -update
	@echo "golden SQL regenerated; review the diff before committing"

# The quickstart. No cloud account, no credentials, no C compiler.
## console: build the UI into the binary's embed directory
console:
	cd console && npm install && npm run build
	@echo "Built. go build ./cmd/truegrain now embeds it."

## console-dev: run the UI with hot reload, proxying the API to a local engine
##
## Two processes: this one serves the pages on 5180, and you run
## `truegrain serve console` on 8080 for the data. Same origin through
## Vite's proxy, so the session cookie and the CSRF header behave exactly
## as they do in production.
console-dev:
	cd console && npm install && npm run dev

demo: build
	@command -v duckdb >/dev/null 2>&1 || { \
		echo "demo needs the duckdb binary: https://duckdb.org/docs/installation/"; exit 1; }
	@mkdir -p build
	@rm -f $(DB)
	duckdb $(DB) < testdata/fixtures/seed.sql
	@echo
	@echo "=== revenue by region ==="
	./$(BIN) query -models $(MODEL) -db $(DB) \
		-metric order_revenue,order_count -dim customers.region -order 'order_revenue:desc'
	@echo
	@echo "=== the same question grouped by a dimension that would inflate it ==="
	@./$(BIN) query -models $(MODEL) -db $(DB) \
		-metric order_revenue -dim order_lines.item_id || true

# The two-team workspace. Shows composition, a cross-namespace import, and a
# grant written by one team following its column into the other.
teams: build
	@command -v duckdb >/dev/null 2>&1 || { echo "teams needs duckdb; run: make duckdb"; exit 1; }
	@mkdir -p build
	@rm -f $(DB)
	@duckdb $(DB) < testdata/fixtures/seed.sql
	@echo "=== the workspace ==="
	./$(BIN) validate -models $(WS)
	@echo
	@echo "=== marketing attributes revenue using the orders dataset sales exported ==="
	./$(BIN) query -models $(WS) -db $(DB) 		-metric marketing.attributed_revenue,marketing.campaign_spend 		-dim marketing.campaigns.channel -order 'attributed_revenue:desc'
	@echo
	@echo "=== metrics from two namespaces, aggregated separately and joined ==="
	./$(BIN) query -models $(WS) -db $(DB) 		-metric sales.order_revenue,marketing.campaign_spend 		-dim sales.orders.order_date -grain month -order 'order_date' -limit 4
	@echo
	@echo "=== the sales grant follows its column into marketing ==="
	-@./$(BIN) compile -models $(WS) -policy testdata/workspace-policy.yaml 		-identity growth@acme.com -metric marketing.attributed_revenue

validate: build
	./$(BIN) validate -models $(MODEL) -v

compile: build
	./$(BIN) compile -models $(MODEL) \
		-metric order_revenue -dim customers.region,orders.order_date -grain month

health: build
	./$(BIN) health -models $(MODEL) -policy testdata/policy.yaml

mcp: build
	./$(BIN) serve mcp -models $(MODEL) -db $(DB)

rest: build
	./$(BIN) serve rest -models $(MODEL) -db $(DB) -addr 127.0.0.1:8080

clean:
	rm -rf bin build
