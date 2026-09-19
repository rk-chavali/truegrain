## What this changes

<!-- One or two sentences. What is different afterwards. -->

## Why

<!-- The problem. If it is a wrong number, paste the compiled SQL and the
     model_version from the response. -->

## Checklist

- [ ] `make test` passes, and `make test-all` if the change touches planning or emission
- [ ] New behaviour has a test that **fails** if the behaviour regresses
- [ ] `make golden` run and the diff reviewed, or no compiled SQL changed
- [ ] `gofmt` and `go vet` clean
- [ ] Docs updated, including `internal/osi/COMPLIANCE.md` if Ossie support changed
- [ ] Commits signed off (`git commit -s`)

## Does this change a compiled result?

- [ ] No
- [ ] Yes, and the diff in `testdata/golden` is in this PR, and an RFC exists

<!-- A change to the compiled output of an unchanged model is a major version
     bump even when the API is untouched. This engine's output is numbers people
     make decisions on. -->
