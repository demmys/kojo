@echo off
cd web && call npm install && call npm run build && cd ..
go build -o kojo.exe ./cmd/kojo
