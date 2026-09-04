# Support

GitHub issues here are for **defects and feature requests in Stellar Index
itself**. This page is for everything else, and points each kind of question
at whatever answers it soonest.

## "How do I…" / "What does this field mean?"

Two references answer most of these, and answer them faster than a thread
would:

- **[docs/reference/](docs/reference/)** — the generated API reference. It is
  produced from `openapi/stellar-index.v1.yaml` and CI fails if the two drift,
  so it describes the API that is actually deployed.
- **[docs/methodology/](docs/methodology/)** — how prices, volumes, supply and
  coverage are computed. Most "this number looks wrong" questions are answered
  here, because the number is usually right and computed differently from what
  was assumed.

If neither answers it, open an issue and say up front that it is a question
rather than a defect. A question that had to become an issue is usually a
documentation gap, so the docs get fixed and the issue closed with the answer.

A discussion forum is the better home for this, and is not switched on for
this repository yet —
[docs/operations/external-channels.md](docs/operations/external-channels.md)
records what turning it on takes.

## "This number looks wrong"

Read the methodology page for that surface first, then check the response
itself: every response carries `as_of` and a `flags` object, and a stale or
withheld value says so there rather than silently serving something plausible.

If it still looks wrong, open a **Data correctness** issue with the request,
the full response, and — most usefully — an independent source for the value
you expected.

## "Is it down?"

- **[stellarindex.io/status](https://stellarindex.io/status)** — live service
  status, latency against the published targets, and any active incident.
- **`GET /v1/status`** — the same verdict as JSON.
- **`GET /v1/coverage`** — whether the data itself is complete, which is a
  different question from whether the service is up.

## Security

**Do not open a public issue.** Follow [SECURITY.md](SECURITY.md).

## Contributing a fix

[CONTRIBUTING.md](CONTRIBUTING.md) for the workflow and the Definition of
Done; [AGENTS.md](AGENTS.md) for the rules a change has to satisfy.

## Commercial and partnership enquiries

The contact addresses in [README.md](README.md).
