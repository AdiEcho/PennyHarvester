.PHONY: dev-backend dev-frontend test test-live test-e2e build run clean

dev-backend:
	cd backend && go run ./cmd/server

dev-frontend:
	cd frontend && npm run dev

test: frontend/node_modules
	cd backend && go test -race ./... && go vet ./...
	cd backend && go test -tags live -run '^$$' ./internal/predictfun
	cd frontend && npm run typecheck && npm run test && npm run build

# Explicit network smoke tests. Requires internet and lives outside the offline
# suite used by CI.
test-live:
	cd backend && PREDICTFUN_LIVE=1 go test -tags live ./internal/predictfun -run 'TestLiveProvider' -v

# Deterministic browser smoke against the built frontend and a mock API. No
# backend service, production database or credentials involved.
test-e2e: frontend/node_modules
	cd frontend && npm run test:e2e

# Install frontend dependencies when they are missing or lockfile changed.
frontend/node_modules: frontend/package.json frontend/package-lock.json
	cd frontend && npm install --no-audit --no-fund

build: frontend/node_modules
	cd frontend && npm run build
	cd backend && go build -o bin/pennyharvester ./cmd/server

# Build the latest frontend and backend, then serve everything from one port.
run: build
	cd backend && WEB_DIR=../frontend/dist ./bin/pennyharvester

clean:
	rm -rf backend/bin frontend/dist
