# Development

Go 1.26 or later.

## Build

```sh
go build -o teamflow ./cmd/teamflow
./teamflow --version
```

## Test

What CI runs, on Linux and macOS:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
shellcheck install.sh
go run ./tools/gen && git diff --exit-code   # generated code (*_gen.go) is current; never edit it by hand
```

`protocol/` is exported from the Teamflow server; after changing it, run `go run ./tools/gen`.

## Release

Pushing a tag `v*` is the release: `.github/workflows/release.yml` tests, builds the archives with GoReleaser,
signs `checksums.txt` with the release key, verifies it and publishes the release. Only maintainers can push
release tags. How to check a release: [VERIFY.md](VERIFY.md).

## The README's clip

`docs/demo.gif` is recorded with [VHS](https://github.com/charmbracelet/vhs) from `docs/demo.tape`, against the real
Teamflow and the latest release, as a demo account, in a throwaway home made by `docs/demo-setup.sh` (the steps are at
the top of the tape).
