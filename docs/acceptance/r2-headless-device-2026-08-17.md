# Cloudflare R2 headless second-device acceptance

This is the external acceptance procedure for using a server as a second
AgentSync device when the server does not have Claude Code installed. It uses
only a dedicated R2 bucket or prefix and synthetic/temporary project data.
Do not place credentials, invitation files, Recovery Keys, session JSONL, or
real conversation content in this repository or in an acceptance report.

## Scope and expected boundary

The server device is a normal AgentSync device with its own device ID. It can
join the managed sync domain, authenticate to R2, inspect encrypted device and
project metadata, and run metadata-only checks. It does not need Claude Code
for those operations.

The server cannot run `resume`, restore a session into an Agent directory, or
continue a conversation until a supported Agent is installed on that machine.
That is an intentional product boundary, not an R2 failure. `doctor` should
report the backend independently from the Agent state:

~~~json
{
  "backend": {"status": "passed"},
  "agent": {"installed": false, "hook": "not-installed"}
}
~~~

## Prerequisites

- a dedicated R2 bucket, or a unique prefix in a test bucket;
- a short-lived R2 S3 credential with access limited to that bucket/prefix;
- the same AgentSync binary build on device A and the server;
- the R2 S3 endpoint `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` and signing
  region `auto`;
- a passphrase shared out of band for the test domain;
- a temporary project directory. The server does not need Claude Code or Git.

Run `init` once with the non-secret R2 settings. AgentSync persists the Remote
endpoint, bucket, region, prefix and path-style choice in `config.json`. If
the encrypted backend credentials do not exist yet, `init` prompts for the
access key, secret key and optional session token, then stores them in the
encrypted local `secrets` file.

## Device A: create the R2 domain

Use a fresh prefix for each run, for example
`acceptance/r2-20260817-01`. Run the initialization command on device A; it
prompts for backend credentials and the domain passphrase, then persists the
configuration locally:

~~~bash
agentsync init --backend s3 \
  --endpoint https://<ACCOUNT_ID>.r2.cloudflarestorage.com \
  --bucket <BUCKET_NAME> \
  --region auto \
  --prefix acceptance/r2-20260817-01 \
  --device-name r2-source \
  --no-hook
~~~

`--no-hook` keeps this acceptance run from changing the source Agent's hook
configuration. The command still performs the backend probe and creates the
managed domain keyfile. Save the printed Recovery Key securely for the test;
do not record it in Git.

Create a signed invitation and transfer it to the server through an
out-of-band channel:

~~~bash
agentsync device invite --output agentsync-invite.json
~~~

Bind the temporary project. A manual identity makes the test independent of
Git and must be reused exactly on the server:

~~~bash
agentsync project bind --name r2-acceptance-20260817-01 --path /path/to/project
~~~

On a source installation that already uses a stable Git identity, the same
identity can instead be bound explicitly on the server with
`agentsync project bind --identity VALUE --path /path/to/project`.

From the temporary project, create the synthetic/temporary source activity
using the normal Agent workflow and publish it explicitly:

~~~bash
cd /path/to/project
agentsync push
~~~

The source device writes only its own device branch. It does not download the
branch it just published.

## Server: join without Claude Code

Use a separate configuration directory for the server device. This is useful
when the server is simulated on the same host, and is also a clear way to
avoid accidentally reusing device A's local identity:

~~~bash
export AGENTSYNC_CONFIG_DIR=/srv/agentsync-r2-20260817-01

agentsync init --invite /srv/incoming/agentsync-invite.json \
  --device-name r2-server \
  --no-hook
~~~

The invite carries the Remote settings and non-secret domain fingerprint, but
not credentials, the passphrase, or session data. The server still needs the
domain passphrase during initialization so it can enroll its own managed
device grant.

Create or enter a temporary directory with no `.git` directory and bind the
same project identity:

~~~bash
mkdir -p /srv/agentsync-r2-20260817-01/project
cd /srv/agentsync-r2-20260817-01/project
agentsync project bind --name r2-acceptance-20260817-01 --path .
~~~

Run the following checks. The server should not prompt for the passphrase
after invitation enrollment because its local device grant is used for reads:

~~~bash
agentsync doctor --json
agentsync device list --json
agentsync status --remote --json
agentsync list --json
agentsync pull --check --json
~~~

Expected results:

- `doctor.backend.status` is `passed` and `doctor.agent.installed` is
  `false`;
- `device list` contains both source and server devices, with only the server
  item marked `local: true`;
- `status --remote`, `list`, and `pull --check` complete successfully and
  report the source metadata without reading shard bodies or writing Agent
  files;
- the project scope is the explicitly bound temporary identity, not the
  server's absolute path and not every project in the domain.

If the source published a session before the server joined, the server can
see its encrypted metadata but must not be expected to resume it. A second
acceptance run with Claude Code installed on the target is required to verify
body download, path localization, restore safety checks, and native resume.

## R2 object and cleanup rules

Use a unique prefix per run. The opt-in `TestS3Integration` test cleans up only
the exact objects it successfully wrote during that invocation; it never
performs a bucket-wide delete. For this two-device procedure, remove only the
dedicated test prefix after collecting results, using the R2 console or a
provider-side lifecycle rule. Do not use a broad delete command against a
shared bucket.

Record the date, commit, operating systems, R2 endpoint family, prefix,
device modes, project identity mode, and redacted command results. Never
record credential values, object bodies, invitation contents, or Recovery
Keys.

## References

- [Cloudflare R2 S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)
- [Cloudflare R2 with the AWS SDK for Go](https://developers.cloudflare.com/r2/examples/aws/aws-sdk-go/)