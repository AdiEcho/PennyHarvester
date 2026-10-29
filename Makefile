.PHONY: dev-backend dev-frontend test build run clean

dev-backend:
	cd backend && go run ./cmd/server

dev-frontend:
	cd frontend && npm run dev

test: frontend/node_modules
	cd backend && go test -race ./... && go vet ./...
	cd frontend && npm run build

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
