# Hackathon POC Context (Juju Repo): No-Controller Dashboard Bridge

## Alignment source

This file is intentionally aligned with the dashboard repo context in:

- /Users/urvashisharma/Desktop/juju-dashboard/docs/hackathon-bootstrap-poc-context.md

If this file and the dashboard file diverge, update both in the same PR window.

## POC goal

When no controller exists, Juju should launch a bootstrap UI experience and provide a localhost bridge endpoint that starts bootstrap and returns a real dashboard URL for auto-handoff.

## Non-goals (strict)

- No production-grade hardening.
- No job IDs.
- No progress streaming.
- No generalized provider matrix.
- No broad command framework refactor.
- No tests — focus on implementation only.

## Confirmed constraints

- Existing controller API facades are unavailable before a controller exists.
- Bootstrap is CLI-side logic in Juju.
- Day-0 flow must be Juju-local bridge driven, not controller-facade driven.

## User flow

1. User runs Juju dashboard command.
2. Juju checks if a controller is available.
3. If controller exists, keep normal behavior.
4. If no controller exists:
   - start localhost bridge server,
   - open bootstrap UI URL,
   - UI submits bootstrap request to localhost.
5. Juju runs bootstrap path and dashboard setup steps.
6. Juju returns dashboard URL.
7. UI redirects automatically.

## Juju POC responsibilities

- Command entrypoint and no-controller mode detection.
- Localhost HTTP bridge.
- Token verification for bridge requests.
- Single endpoint for bootstrap run.
- Synchronous bootstrap invocation for POC.
- Return destination dashboard URL on success.

## Minimal API contract

### POST /bootstrap/run

Headers:

- Authorization: Bearer <token>

Request JSON:
{
"cloud": "aws",
"region": "us-east-1",
"controllerName": "ctrl-1",
"dashboardType": "machine",
"credentialName": "default"
}

Success JSON:
{
"ok": true,
"dashboardUrl": "https://example-dashboard/",
"controllerName": "ctrl-1"
}

Failure JSON:
{
"ok": false,
"error": "bootstrap failed: reason"
}

## Security minimum for POC

- Bind bridge server to localhost only.
- Require short-lived bearer token.
- Enforce one in-flight bootstrap request at a time.

## Suggested implementation slices (Juju-only)

1. Add or extend Juju dashboard command with no-controller branch.
2. Start localhost bridge with token middleware.
3. Implement POST /bootstrap/run handler.
4. Wire handler to existing bootstrap path.
5. Run dashboard deploy/integrate/expose for selected mode.
6. Resolve and return dashboard URL.
7. Open bootstrap UI URL in browser with bridge base URL and token.

## Cut list if time slips

- Limit to one cloud/provider for demo.
- Hardcode minimal defaults where safe.
- Skip retries and advanced diagnostics.
- Keep only happy path plus one clear failure response.

## Definition of done (hackathon)

- Running Juju dashboard in a no-controller environment opens bootstrap UI.
- UI can call POST /bootstrap/run successfully.
- Bootstrap path is invoked from Juju bridge.
- On success, response includes dashboardUrl for auto-handoff.

## Starter prompt for model opened in Juju repo

Use this instruction in a fresh model window:

Implement the POC in this file only. Keep scope strict: no jobs, no streaming, no production hardening. Add or extend the Juju dashboard command to support no-controller mode, run a localhost bridge with token auth, implement POST /bootstrap/run, invoke existing bootstrap path, and return dashboardUrl for UI auto-handoff. First output a file-by-file plan, then implement in small commits.
