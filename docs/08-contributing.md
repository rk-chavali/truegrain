# 08 Contributing and open source operating plan

How the project runs in public so that contributions are possible and the
repository stays explainable.

## Licensing

- **Apache 2.0.** Same as OSI itself. Permissive, patent grant included, and the
  license enterprises accept without a legal review cycle.
- **DCO, not a CLA.** A sign-off line in each commit is far lower friction than
  a CLA and is enough for a project at this stage. Enforce it with the DCO
  GitHub app.

## Repository hygiene

This was a checklist to complete before going public. It is done, and the
repository is public. Kept as a record of what is expected to stay in place.

- `README.md`, with a quickstart that works on DuckDB with no credentials
- `LICENSE`, `CODE_OF_CONDUCT.md`, `SECURITY.md` with a disclosure address
- `CONTRIBUTING.md` pointing at this file
- Issue templates: bug, feature, model correctness report
- A pull request template
- `docs/`, this folder

Still missing: a demo recording, and a badge for the parity suite.

## Quickstart

It runs with no cloud account and no credentials.

```bash
git clone https://github.com/rk-chavali/truegrain
cd truegrain
make demo        # loads fixture data into DuckDB, runs one query end to end
```

Or without cloning anything:

```bash
docker run --rm -v "$PWD:/models" ghcr.io/rk-chavali/truegrain:v0.3.0   validate -models /models
```

`make demo` needs the DuckDB binary, which is a single executable with no
installer. `make duckdb` downloads it into `bin/`. Everything except `query`
and `serve` needs no database at all.

Anything that requires a GCP project before a first successful query loses most
visitors at that step, which is why DuckDB is the default dialect rather than
BigQuery.

## Contribution flow

1. Open an issue first for anything beyond a typo or a small fix.
2. Maintainer triages within a stated window and applies labels.
3. PR references the issue, includes tests, passes CI.
4. One maintainer approval merges.

Labels that actually get used: `good first issue`, `dialect`, `planner`,
`governance`, `osi-compliance`, `correctness`, `needs-repro`.

Seed at least ten genuine `good first issue` items before launch. An empty issue
list reads as a project with no room for anyone else.

## RFC process

Required for: changes to the plan object, new OSI construct support, anything
that changes a compiled SQL result, and any change to the governance contract.

- A markdown file in `rfcs/`, numbered
- Sections: problem, proposal, alternatives considered, compatibility impact,
  test plan
- Open for comment for a stated period before a decision
- Decision recorded in the file, not only in the PR thread

This is heavier than a project this size normally needs. It is worth it for one
reason: a semantic layer's output is numbers people make decisions on, and an
undocumented change to a compiled result is a silent correctness incident.

## Maintainer model

- Start with two maintainers. A single-maintainer project reads as abandoned the
  first time you take a week off.
- Document what a maintainer does and how someone becomes one.
- `CODEOWNERS` for the planner and governance packages specifically, since those
  are where a bad merge is most expensive.

## Release process

- Semantic versioning. Anything that changes a compiled SQL result for an
  unchanged model is a **major** version bump, even if the API is unchanged.
  This is stricter than normal semver and it is the right call here.
- Changelog per release, with a dedicated section for changes that affect
  results.
- Tagged releases produce: static binaries for Linux, macOS and Windows, a
  container image, and the Python SDK on PyPI.

## OSI relationship

- Maintain `internal/osi/COMPLIANCE.md` listing every spec construct and its
  status: supported, unsupported, or intentionally diverged with a reason.
- File issues upstream against the OSI repository when the spec is ambiguous.
  Being a visible, good-faith implementer is worth more than any single feature,
  and it is what turns this project into a reference implementation rather than
  one more tool.
- If the spec and this engine disagree, the spec wins and the divergence is
  documented.

## Launch

- Publish when the DuckDB quickstart works and the demo video exists. Not
  before, not much after.
- Write-up goes to the Google Cloud Community publication on Medium and to
  `discuss.google.dev`, structured as: the concrete gap, what BigQuery gives
  today, where it falls short, the working implementation, and what native
  support should look like.
- Cross-post the announcement, but expect the real audience to be in the cloud
  and data communities rather than a general professional feed.
- Link the repo from any related issue tracker thread.

## Measuring whether it worked

Track and publish, monthly:

- unique clones and repo traffic
- issues opened by people who are not you
- OSI models in the wild that run against the engine
- external PRs merged

The last two are the only ones that mean anything. Stars are a vanity metric and
should not influence a single roadmap decision.
