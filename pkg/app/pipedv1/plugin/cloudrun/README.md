![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)
[![Built on PipeCD](https://img.shields.io/badge/Built%20on-PipeCD-informational)](https://github.com/pipe-cd/pipecd)
[![PipeCD Docs](https://img.shields.io/badge/Docs-pipecd.dev-blue)](https://pipecd.dev/docs/)
![Originally proposed for GSoC 2026](https://img.shields.io/badge/Originally%20proposed%20for-GSoC%202026-e8702a)

# Cloud Run Plugin for PipeCD v1

A platform plugin that lets [PipeCD](https://github.com/pipe-cd/pipecd) — a CNCF GitOps continuous delivery
platform — deploy applications to [Google Cloud Run](https://cloud.google.com/run), built against PipeCD's
new v1 plugin architecture.

> **This is a fork of [pipe-cd/pipecd](https://github.com/pipe-cd/pipecd).** The plugin lives entirely under
> [`pkg/app/pipedv1/plugin/cloudrun/`](.) and doesn't modify anything else in the upstream project. Everything
> outside that folder is the original PipeCD codebase, included here only so the plugin has somewhere to live
> and build against the real plugin SDK.

---

## Where this came from

This plugin started as my proposal for Google Summer of Code 2026. The proposal — included in this folder
as [`docs/proposal.pdf`](./docs/proposal.pdf) — wasn't accepted; another contributor's was a better fit that
year. Rather than shelve the idea, I decided to build it anyway, on my own time, mostly to learn Go and cloud
infrastructure properly by building something real, rather than through tutorials.

I came into this with no prior Go experience and only a surface-level understanding of cloud deployment
internals. Everything below — the architecture, the client wrapper, the deployment stages, the integration
tests — was built up incrementally, one concept at a time, with a lot of reading PipeCD's existing plugins
(particularly the Kubernetes plugin) to understand the patterns before writing anything Cloud-Run-specific.

The proposal's phased plan held up better than I expected. Phases 1–4 described in it are implemented and
tested here; the comparison between what was proposed and what actually got built is, I think, the most
honest way to show how the project went.

---

## What it does

The plugin implements PipeCD's three core plugin services for Cloud Run:

| Service | What it does |
|---|---|
| **Deployment** | Deploys a new revision from a `service.yaml` manifest, waits for it to become healthy, then shifts 100% of traffic to it. Also supports rolling back to the previously running revision. |
| **LiveState** | Reports the current state of the Cloud Run service and its revisions back to PipeCD, so the UI can show what's actually running and detect drift. |
| **Versions** | Reads the container image tag out of the manifest so PipeCD can show what version is being deployed. |

### Architecture, in short

PipeCD v1 splits responsibilities between the **core** (orchestration, state machine, deployment strategy)
and **plugins** (platform-specific execution). This plugin only ever does the latter: it doesn't decide
*whether* to roll out, retry, or roll back — it just executes the stage it's told to run and reports back
what happened. `DetermineStrategy` deliberately returns `nil` for this reason, deferring to the core's own
logic instead of adding Cloud-Run-specific opinions about deployment strategy.

```
pkg/app/pipedv1/plugin/cloudrun/
├── main.go              # plugin entrypoint (gRPC server registration)
├── client/               # thin wrapper around the Cloud Run API
│   ├── client.go          # Create / Update / List / GetRevision / ListRevisions
│   ├── cloudrun.go        # Service/Revision types, health-condition parsing, label helpers
│   └── servicemanifest.go # service.yaml parsing and editing (revision name, traffic, labels)
├── config/                # plugin configuration types (CloudRunApplicationSpec, DeployTargetConfig)
├── deployment/             # DetermineVersions / DetermineStrategy / ExecuteStage
│   ├── sync.go             # CLOUDRUN_SYNC stage — deploy, wait for ready, switch traffic
│   └── rollback.go         # ROLLBACK stage — re-deploys the previous manifest via the same logic
└── livestate/              # GetLivestate — maps live Cloud Run state into PipeCD's resource model
```

A Cloud Run **Service** maps to a PipeCD application; each Cloud Run **Revision** maps to a PipeCD version.
The deployment flow ("direct strategy", in the proposal's terms) is: create or update the service, poll the
new revision until Cloud Run reports it healthy, then switch all traffic to it. Both `CLOUDRUN_SYNC` and
`ROLLBACK` share the same underlying logic — a rollback is just a redeploy of an older manifest — so there's
one code path to trust instead of two.

---

## Testing

Alongside the plugin code, this includes integration tests that exercise the real Cloud Run API — not just
mocks. They're tagged `integration` and excluded from a normal `go test ./...`, since they need real GCP
credentials and create real (if free-tier-eligible) cloud resources:

```bash
export CLOUDRUN_TEST_PROJECT=your-gcp-project
export CLOUDRUN_TEST_REGION=europe-west1
export CLOUDRUN_TEST_CREDENTIALS_FILE=/path/to/service-account-key.json
export CLOUDRUN_TEST_APP_DIR=/path/to/folder/containing/service.yaml

go test -tags=integration ./... -run TestIntegration -v
```

These tests deploy a real service, wait for a real revision to become healthy, switch real traffic, and
roll back — the same path a live PipeCD deployment would take. Running them against the live API is also how
a couple of real issues got caught that reading the code alone wouldn't have shown, including:

- Cloud Run requiring an explicit `iam.serviceAccounts.actAs` grant on the default compute service account
  before it will create a service on your behalf — a one-time GCP project setup step, not a plugin bug.
- The exact error returned when creating a service that already exists (`409 Conflict`), which informed how
  `ExecuteStage` now distinguishes "this service already exists, update it instead" from a genuine failure,
  rather than retrying blindly on every error.

---

## Status

| Proposal phase | Status |
|---|---|
| Phase 1 — Plugin setup | ✅ Done |
| Phase 2 — LiveState | ✅ Done, integration-tested |
| Phase 3 — Deployment (MVP, direct strategy) | ✅ Done, integration-tested |
| Phase 4 — Reliability | 🟡 Partial — Create/Update conflict handling done; revision-readiness timeout in place; `client.Delete` not yet implemented |
| Phase 5 — SDK improvements | 📝 Noted, not pursued as an upstream PR (see below) |
| Phase 6 — Docs & blog | 🟡 This README is most of it |

One observation from Phase 5 worth recording: `sdk.DeploymentSource` has a convenient `AppConfig()` helper
for reading the parsed application config, but no equivalent helper for reading an arbitrary manifest file
(like `service.yaml`) from the deployment's source directory — every plugin that needs one seems to end up
writing its own small `os.ReadFile` + path-joining helper. That's a small, plausible candidate for an SDK
improvement, though I haven't turned it into an upstream PR.

---

## Acknowledgements

Built against [PipeCD](https://github.com/pipe-cd/pipecd), a CNCF Sandbox project — thanks to the PipeCD
maintainers for a codebase that was genuinely possible to learn from by reading it. Particular thanks to
mentors Khanh Tran ([@khanhtc1202](https://github.com/khanhtc1202)) and Shinnosuke Sawada-Dazai
([@Warashi](https://github.com/Warashi)) for the original proposal review.

If you'd like to know more about how this was built, I wrote about the process here: **[LinkedIn post — add link]**
