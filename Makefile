BINARY  := seedora
PKG     := ./cmd/seedora
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: help build dev dev-big dev-mysql test test-js test-browser test-all race cover vet fmt lint bench pages clean demo demo-big mysql-up mysql-down pg-up pg-down

help:
	@echo "build   single binary with the UI embedded"
	@echo "dev     run against the demo database, UI on :7777"
	@echo "dev-big run against the large demo schema, UI on :7777"
	@echo "mysql-up start a throwaway MySQL in Docker, on :13306"
	@echo "dev-mysql run against that MySQL, seeding the example schema's migrations"
	@echo "mysql-down remove it"
	@echo "test    unit tests"
	@echo "test-js the page's router and layout, under node"
	@echo "test-browser the page in a real browser (needs Playwright, see tests/browser)"
	@echo "test-all everything above, plus the integration tests against the throwaway servers"
	@echo "pg-up   start a throwaway PostgreSQL in Docker, on :15432"
	@echo "pg-down remove it"
	@echo "race    unit tests under the race detector"
	@echo "cover   tests with a coverage report"
	@echo "bench   generation and insert throughput"
	@echo "pages   build the static demo published to GitHub Pages"
	@echo "vet     go vet"
	@echo "fmt     gofmt every file"
	@echo "demo    build a throwaway SQLite database to point Seedora at"
	@echo "demo-big build the large demo database"

# The UI is embedded with go:embed, so there is no asset pipeline and no
# separate build step — one command produces the shippable artefact.
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

dev: demo
	go run $(PKG) --dsn ./tmp/demo.db --config ./tmp/demo.yaml

dev-big: demo-big
	go run $(PKG) --dsn ./tmp/demo20.db --config ./tmp/demo20.yaml

test:
	go test ./...

# The diagram is the largest part of the codebase and its geometry is plain
# functions, so it is tested the same way everything else is. node's own test
# runner, no dependencies, no package.json: the point of this project is not
# having a JavaScript toolchain.
test-js:
	node --test tests/ui/

# The page in a real Chromium: the layout as the browser computes it, the drag
# gestures, and a seeding run driven from the UI. This is the only target with
# a dependency list, and it is deliberately outside the build — see
# tests/browser/requirements.txt. PYTHON points at the interpreter holding
# them.
PYTHON ?= .venv/bin/python

test-browser:
	@test -x "$(PYTHON)" || { \
		echo "no interpreter at $(PYTHON) — see tests/browser/requirements.txt,"; \
		echo "or pass one: make test-browser PYTHON=/path/to/python"; exit 1; }
	$(PYTHON) -m pytest tests/browser/ -q

# Everything, including the integration tests that skip without a server. Both
# containers have to be up; `make pg-up mysql-up` first.
test-all: test-js
	SEEDORA_TEST_POSTGRES=postgres://seedora:seedora@127.0.0.1:15432/seedora_test?sslmode=disable \
	SEEDORA_TEST_MYSQL=mysql://root:seedora@127.0.0.1:13306/seedora_dev \
	go test ./...

# The Postgres counterpart to mysql-up. The integration tests in tests/ skip
# without SEEDORA_TEST_POSTGRES, which on a laptop means they are usually not
# run at all; this is the one line that fixes that.
pg-up:
	docker run -d --name seedora-postgres \
		-e POSTGRES_PASSWORD=seedora -e POSTGRES_USER=seedora -e POSTGRES_DB=seedora_test \
		-p 15432:5432 postgres:16-alpine
	@echo "waiting for PostgreSQL…"
	@until docker exec seedora-postgres pg_isready -U seedora -q 2>/dev/null; do sleep 1; done
	@echo "postgres://seedora:seedora@127.0.0.1:15432/seedora_test?sslmode=disable"

pg-down:
	docker rm -f seedora-postgres

# A throwaway MySQL to develop and test the driver against. The port is not
# 3306, so it cannot collide with one already running on the machine.
mysql-up:
	docker run -d --name seedora-mysql \
		-e MYSQL_ROOT_PASSWORD=seedora -e MYSQL_DATABASE=seedora_dev \
		-p 13306:3306 mysql:8
	@echo "waiting for MySQL…"
	@until docker exec seedora-mysql mysqladmin ping -h 127.0.0.1 -pseedora --silent 2>/dev/null; do sleep 1; done
	@echo "mysql://root:seedora@127.0.0.1:13306/seedora_dev"

mysql-down:
	docker rm -f seedora-mysql

# The migrations flag is the point of this target: an empty database and a
# directory of .sql files is the state a fresh checkout is in.
dev-mysql:
	go run $(PKG) --dsn mysql://root:seedora@127.0.0.1:13306/seedora_dev \
		--migrations ./examples/mysql-migrations/migrations \
		--config ./examples/mysql-migrations/seedora.yaml

# Generation runs on a worker pool, so the race detector is not optional here.
race:
	go test -race ./...

# The Pages demo is generated, never hand-maintained: it copies the UI's own
# files and records the API's answers by running the tool against the example
# schema, so it cannot drift from the product.
pages:
	go run ./tools/pages
	@echo "docs/ ready — serve it with: python3 -m http.server -d docs 8080"

# Benchmarks live outside internal/ so `go test ./...` stays fast; they are run
# on purpose or not at all.
bench:
	go test ./benchmarks/ -bench . -benchtime 3s -run XXX

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: fmt vet race test-js

clean:
	go clean
	rm -rf $(BINARY) coverage.out tmp

# demo builds a small schema worth seeding: a foreign key, a unique column with
# a length limit, and a nullable one, which is enough to exercise every path.
demo:
	@mkdir -p tmp
	@test -f tmp/demo.db || sqlite3 tmp/demo.db "\
CREATE TABLE users ( \
  id INTEGER PRIMARY KEY, \
  email VARCHAR(60) NOT NULL UNIQUE, \
  first_name VARCHAR(50) NOT NULL, \
  last_name VARCHAR(50) NOT NULL, \
  city VARCHAR(60), \
  phone VARCHAR(30), \
  company VARCHAR(80), \
  is_active BOOLEAN NOT NULL, \
  created_at TIMESTAMP NOT NULL); \
CREATE TABLE orders ( \
  id INTEGER PRIMARY KEY, \
  user_id INTEGER NOT NULL REFERENCES users(id), \
  status VARCHAR(20) NOT NULL, \
  total DECIMAL(10,2) NOT NULL, \
  placed_at TIMESTAMP NOT NULL);"
	@echo "tmp/demo.db ready"

# demo-big is the same idea at the size a real schema reaches. Two tables prove
# the seeder runs; they prove nothing about the layout, the edge routing, or the
# colour scheme. It also carries all four cardinalities, which are each drawn
# differently and so are each a way to get the diagram wrong.
demo-big:
	@mkdir -p tmp
	@test -f tmp/demo20.db || sqlite3 tmp/demo20.db < testdata/demo_large.sql
	@echo "tmp/demo20.db ready"
