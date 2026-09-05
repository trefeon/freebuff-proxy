## Description

What does this change do and why? Link any related issue.

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor / internal
- [ ] Docs only
- [ ] Dependency update

## Checklist

- [ ] Code builds: `go build ./...`
- [ ] `gofmt` clean, `go vet ./...` clean
- [ ] Tests pass: `task test` hermetic (CI also runs `-race`)
- [ ] Tests added/updated in the owning package, behavior-first
- [ ] Behavior/protocol/config changes documented (`README.md`, `docs/api/`, config tables)
- [ ] Architecture: no new import edges (`archtest` green); CONTRACT.md/ADR updated if ownership moved
- [ ] Security: no secrets in the diff; admin/auth/error paths reviewed; redaction intact
- [ ] Generated assets rebuilt if frontend changed (`dashboard/dist` fresh)
- [ ] Upstream reference involved? SHA recorded; pins + parity updated

## Notes

Anything reviewers should know (upstream API quirks, verification on live
traffic, etc.)