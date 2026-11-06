# Contributing

## Build and test

You need the Go version in `go.mod`.

```sh
go build -o line-lx .
gofmt -l .
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

## Pull requests

- Keep each pull request to one change, and explain why it's needed.
- Add tests for any change in behaviour.
- Update `README.md` and `CHANGELOG.md` when users would notice the change.
- CI must pass: `gofmt -l .` prints nothing, and vet, tests and govulncheck succeed.

To report a vulnerability, see [SECURITY.md](SECURITY.md).
