# Go Toolchain

This project uses **Go 1.26.6** (stable release).

## Verification

To verify the installed Go version:

```sh
go version
```

## Build

```sh
make build         # or: go build ./...
make install       # builds + installs the binary
```

## Test

```sh
make test          # go test ./...
make race          # go test -race ./...
```
