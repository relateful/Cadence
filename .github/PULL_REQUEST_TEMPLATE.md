## What this changes

## Why

## How it was tested

## Checklist

- [ ] The title follows Conventional Commits, for example `fix(voice): …` or `feat(mixer): …`
- [ ] `gofmt -l .` prints nothing and `go vet ./...` is clean
- [ ] `go test ./...` passes
- [ ] Voice changes were also built and tried with `-tags native` on Linux
- [ ] New behaviour is covered by a test
- [ ] README and `.env.example` are updated if configuration or the API changed
