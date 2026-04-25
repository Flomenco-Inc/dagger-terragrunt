# dagger-terragrunt

Shared [Dagger](https://dagger.io) module that wraps `terragrunt run-all`
plan/apply for flo's three terragrunt deployment repos:

- [`flo-account-admin`](https://github.com/Flomenco-Inc/flo-account-admin) —
  per-account bootstrap (quarterly cadence).
- [`flo-core-services`](https://github.com/Flomenco-Inc/flo-core-services) —
  regional core services like DNS, build artefacts, security alerts (monthly).
- [`flo-platform`](https://github.com/Flomenco-Inc/flo-platform) — per-env
  application stacks (every merge to `flo/main`).

## Credential model — OIDC-native

The module **does not accept AWS access keys**. The only credential material
accepted at the public API boundary is:

- `--role-arn` — the IAM role in the target AWS account.
- `--oidc-token` — a short-lived GitHub Actions OIDC JWT (`Secret`).
- `--git-token` — **optional** short-lived GitHub App installation token
  (`Secret`) used to clone private Terraform module repos referenced by
  terragrunt. Mint via `actions/create-github-app-token` in the caller
  workflow. Omit when every referenced module is public.

Inside the Dagger container the module runs
`aws sts assume-role-with-web-identity` and exports the resulting session
credentials as env vars **for the single terragrunt invocation only**. The
session creds never leave the Dagger exec, and the OIDC token is mounted on
a tmpfs (`/run/secrets/oidc-token`) so it is never logged, never persisted
in the Dagger cache, and never exposed as a container env var. If provided,
the git token is mounted on a separate tmpfs (`/run/secrets/gh-token`) and
consumed by an in-memory `git config url.insteadOf` rule so terragrunt's
module downloads authenticate as the `flo-ci` GitHub App installation.

No path accepts `--aws-access-key-id` / `--aws-secret-access-key`. That
would make it syntactically possible to pass a long-lived IAM user key, and
even if every known caller used OIDC-minted session creds in practice, the
signature IS the policy surface. We eliminated the option structurally.

## Functions

| Function    | Credentials                              | Mutates state?        | When to call it                          |
| ----------- | ---------------------------------------- | --------------------- | ---------------------------------------- |
| `validate`  | **none**                                 | no                    | Every PR — fast structural checks.       |
| `plan`      | OIDC role + token (+ optional git-token) | no (read-only APIs)   | Every PR — shows diff, posts to PR body. |
| `apply`     | OIDC role + token (+ optional git-token) | **YES**               | After PR merge + environment approval.   |

### `validate --src=<dir> [--tg-version=v1.0.2] [--tf-version=1.9.8]`

Runs:

- `terragrunt hcl format --check --diff`
- `terraform fmt -check -recursive -diff` across every `.tf` under the tree

No AWS calls. No credentials required. Safe to run on untrusted PRs.

### `plan --src=<dir> --env=<dev|stg|prd> --role-arn=<arn> --oidc-token=env:OIDC_TOKEN [opts]`

Runs `terragrunt --non-interactive run --all plan` scoped to `./<env>/`.

Required args:

- `--src` — terragrunt repo root directory.
- `--env` — env directory name under the repo root (`dev`, `stg`, `prd`).
- `--role-arn` — IAM role ARN to assume. The role must trust the GitHub
  Actions OIDC provider with a `sub` claim matching the caller's repo/branch.
- `--oidc-token` — OIDC JWT minted by `core.getIDToken("sts.amazonaws.com")`.
  Always pass as a Dagger secret (`env:OIDC_TOKEN`) so the plaintext never
  touches the command line.

Optional:

- `--git-token` — GitHub App installation token for private module cloning
  (see [Private module access](#private-module-access) below).
- `--region` (default `us-east-2`)
- `--session-name` (default `dagger-terragrunt`; set to something
  CI-specific like `gha-${{ github.run_id }}` for traceability in CloudTrail)
- `--duration-seconds` (default `900` — matches the intended
  `MaxSessionDuration` on the plan/apply roles; must be ≤ the role's cap)
- `--tg-version`, `--tf-version`
- `--extra-env KEY=VALUE` — repeatable. Forwards the given env var into
  the terragrunt exec environment so HCL can read it via `get_env(...)`.
  Intended for plumbing values that shift between plan and apply — for
  example a `TG_ROLE_VARIANT` flag that a `generate` block uses to pick
  a plan-role vs apply-role ARN for a cross-account provider alias.
  Keys starting with `AWS_` are **rejected** — the module owns that
  namespace (region, access key, session token, etc). Pass sensitive
  values via `--oidc-token` / `--git-token` instead; `--extra-env` is
  **not** a secret surface.
- `--leaf <subpath>` — optional. Scopes terragrunt to a single leaf
  under `<env>/<leaf>` and drops the `run --all` wrapper. When omitted
  (default), behavior is unchanged: `run-all` across the full env
  graph. When set, terragrunt runs plain `plan` / `apply` in the named
  leaf and reads dependency outputs from remote state without
  re-initializing ancestor leaves. See
  [Leaf-scoped redeploy](#leaf-scoped-redeploy) below for when to use
  this. Path guardrails: `..` and leading `/` are rejected.
- `--summarize` (plan only, default `true`) — when true, the verbose
  terraform plan stdout is captured to a container-side log file and a
  compact per-leaf summary is emitted instead. Lists resource-level
  actions (create / update / replace / destroy) with addresses but
  omits attribute diff bodies. See
  [Plan summarization](#plan-summarization) below for why this
  matters. Pass `--summarize=false` to restore the legacy verbose
  stream when debugging an unexpected diff in a small plan.

### `apply --src=<dir> --env=<...> --role-arn=<arn> --oidc-token=env:OIDC_TOKEN [opts]`

Same signature as `plan`, including `--extra-env` and `--leaf`. Runs
`terragrunt --non-interactive run --all apply --auto-approve`, or the
leaf-scoped variant when `--leaf` is set. **This mutates AWS state.**
Gate it on a GitHub Actions environment protection rule with required
reviewers (see CI integration below).

### Plan summarization

`terragrunt run-all plan` calls `terraform plan` per leaf, and
terraform's diff renderer prints the full value of every changed
attribute. That includes multi-line strings — and the API Gateway
leaves in flo-core-services use
`aws_api_gateway_rest_api.body = yamlencode(<merged-openapi>)`, which
grows linearly with the number of services on the API. With ~7
services merged the body diff alone is multiple MB.

GitHub Actions caps step output at ~1 MB and step log streams at a
few MB. Past a handful of services the raw plan output crashes the
runner with `Maximum object size exceeded` before reviewers ever see
the plan. We hit this in flo-core-services around the time we
migrated the third Tier-1 service.

`--summarize=true` (the default) gives the module's stdout a fixed
output budget independent of the plan body. The flow:

1. `terragrunt run-all plan -- -out=tfplan.bin` runs with both
   stdout and stderr redirected to `/tmp/plan.log` inside the
   container — never reaches the dagger output stream.
2. On plan failure, the full captured log is emitted (and exit
   non-zero) — operators always need failure diagnostics.
3. On success, the module walks every `tfplan.bin` produced under
   the env tree, runs `terraform show -no-color -json tfplan.bin`
   per-leaf, and emits a compact summary: per-leaf header,
   create/update/replace/destroy counts, per-resource action lines
   (`address` + action), then totals across all leaves.

Output scales with `O(num_resources_changing)`, not
`O(sum_of_attribute_body_sizes)`. A 5-leaf 200-resource plan emits
roughly 250 lines instead of 30k+.

If the summary ever loses information you need (e.g. a specific
attribute change was the cause of a regression and the diff is
needed for triage), pass `--summarize=false` for that one
invocation to restore the verbose stream.

### Leaf-scoped redeploy

`terragrunt run-all` initializes every leaf under `<env>/` on every
invocation, even when only one leaf's state could have changed. For a
five-leaf dev env that's ~60-90s of pure overhead on every re-apply.
Passing `--leaf=<name>` collapses the call to a single-dir
`terragrunt apply`, reading dependency outputs from remote state
without re-planning ancestors.

When to reach for this:

- Image-only redeploys — the webapp/api/worker leaf picks up a new
  ECR digest on re-apply, but nothing else in the env has changed.
- Tuning a single leaf's inputs post-initial-apply.

When NOT to:

- First apply of a new env. `--leaf` assumes dependencies have
  remote state to read; use run-all for the initial build-out.
- Changes that affect the DAG (adding new deps, reshaping outputs).
  Run-all validates the whole graph; `--leaf` won't catch drift in
  other leaves.

Example — the flo monorepo's webapp container build triggers
leaf-scoped re-applies on the webapp leaf only, avoiding the
run-all overhead on every image bump:

```bash
dagger call -m github.com/Flomenco-Inc/dagger-terragrunt@v0.5.0 \
  apply --src=. --env=envs/dev/us-east-1 --leaf=webapp \
    --role-arn=arn:aws:iam::<acct>:role/gha-core-services-apply \
    --oidc-token=env://OIDC_TOKEN \
    --git-token=env://GH_TOKEN
```

## Why the env split is positional, not auto-detected

Each env directory in the terragrunt repos is a self-contained graph of
modules with its own `root.hcl`. Running `run-all` across the whole repo
root would build a single cross-env graph and (in pathological cases) apply
changes to prd triggered by a dev-only PR. Scoping to one env at a time
removes that class of bug entirely. The trade-off: the caller has to name
the env. That's worth it.

## Tool-version defaults

Defined as constants at the top of `main.go`:

- Terragrunt: `v1.0.2`
- Terraform: `1.9.8`
- AWS region: `us-east-2`
- Session duration: `900` seconds (15 min)

To propose a bump, open a PR that edits the `const (...)` block. Renovate
groups Dagger-module version bumps.

## Local development

`validate` runs natively with no credentials — good for iterating on
HCL formatting locally.

```bash
dagger call -m github.com/Flomenco-Inc/dagger-terragrunt@v0.1.0 \
  validate --src=.
```

`plan` and `apply` **deliberately do not work from a developer laptop** —
they require a GitHub Actions OIDC JWT (`sts.amazonaws.com` audience) that
only GHA can mint. This is intentional; it enforces the policy that
plan/apply happen through a reviewed PR rather than from an ad-hoc shell.

If you need to iterate on plan/apply while developing this module itself,
cut a draft PR on the consuming repo (e.g. flo-account-admin) and let CI
exercise the module against a dev role.

## CI integration

The canonical consumer workflow lives in each terragrunt repo's
`.github/workflows/plan.yml` and `.github/workflows/apply.yml`.

Note: you do **not** need `aws-actions/configure-aws-credentials` any more —
the Dagger module handles the OIDC exchange itself. You only need
`id-token: write` so GitHub will mint the JWT for you.

```yaml
# plan.yml — runs on every PR
name: terragrunt plan
on:
  pull_request:
    branches: [main]
permissions:
  id-token: write        # mint OIDC JWT
  contents: read
  pull-requests: write
jobs:
  plan:
    strategy:
      matrix:
        env: [dev, stg, prd]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5

      - name: Mint OIDC token
        uses: actions/github-script@v7
        with:
          script: |
            const tok = await core.getIDToken("sts.amazonaws.com");
            core.setSecret(tok);
            core.exportVariable("OIDC_TOKEN", tok);

      - name: Mint GitHub App token for private modules
        id: app-token
        uses: actions/create-github-app-token@v2
        with:
          app-id: ${{ vars.FLO_CI_APP_ID }}
          private-key: ${{ secrets.FLO_CI_APP_PRIVATE_KEY }}
          owner: Flomenco-Inc

      - name: Export git token to env
        env:
          GH_TOKEN: ${{ steps.app-token.outputs.token }}
        run: echo "GH_TOKEN=${GH_TOKEN}" >> "$GITHUB_ENV"

      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: |
            call -m github.com/Flomenco-Inc/dagger-terragrunt@v0.2.0 \
              plan --src=. --env=${{ matrix.env }} \
                --role-arn=arn:aws:iam::${{ vars.FLO_DEV_ACCOUNT_ID }}:role/gha-terragrunt-plan \
                --oidc-token=env://OIDC_TOKEN \
                --git-token=env://GH_TOKEN \
                --session-name=gha-${{ github.run_id }}
```

```yaml
# apply.yml — runs on push to main, gated on env approval
name: terragrunt apply
on:
  push:
    branches: [main]
permissions:
  id-token: write
  contents: read
jobs:
  apply:
    strategy:
      matrix:
        env: [dev, stg, prd]
      max-parallel: 1                    # serialize envs
    runs-on: ubuntu-latest
    environment: ${{ matrix.env }}       # <- approvals live here
    steps:
      - uses: actions/checkout@v5

      - name: Mint OIDC token
        uses: actions/github-script@v7
        with:
          script: |
            const tok = await core.getIDToken("sts.amazonaws.com");
            core.setSecret(tok);
            core.exportVariable("OIDC_TOKEN", tok);

      - name: Mint GitHub App token for private modules
        id: app-token
        uses: actions/create-github-app-token@v2
        with:
          app-id: ${{ vars.FLO_CI_APP_ID }}
          private-key: ${{ secrets.FLO_CI_APP_PRIVATE_KEY }}
          owner: Flomenco-Inc

      - name: Export git token to env
        env:
          GH_TOKEN: ${{ steps.app-token.outputs.token }}
        run: echo "GH_TOKEN=${GH_TOKEN}" >> "$GITHUB_ENV"

      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: |
            call -m github.com/Flomenco-Inc/dagger-terragrunt@v0.2.0 \
              apply --src=. --env=${{ matrix.env }} \
                --role-arn=arn:aws:iam::${{ vars.FLO_DEV_ACCOUNT_ID }}:role/gha-terragrunt-apply \
                --oidc-token=env://OIDC_TOKEN \
                --git-token=env://GH_TOKEN \
                --session-name=gha-${{ github.run_id }}
```

The required-reviewers gate sits on the GitHub Actions **environment**
(`dev` / `stg` / `prd`), not on the branch. That's how we enforce the
"two-approver rule for prd applies" from the migration plan. Branch
protection only enforces that commits landed on `main` via PR — approval
to **deploy** is a separate, per-env decision.

### Private module access

Flo's `terraform-aws-*` modules are all private repos. Terragrunt's
`module { source = "git::https://github.com/Flomenco-Inc/..." }` clones
therefore need authentication, which this module delegates to a GitHub
**App** (`flo-ci`) rather than SSH deploy keys or personal access tokens.

Why the App pattern, concretely:

- **Ephemerality.** `actions/create-github-app-token` mints an
  installation token that expires in ≤1 hour. If a CI log somehow leaks the
  token, the blast window is bounded. A PAT would be valid for whatever
  expiry the creator picked (often never).
- **No human bus factor.** Tokens aren't tied to a person's account. When
  someone leaves the org, App installations keep working. SSH deploy keys
  and PATs don't.
- **Installation-scoped.** The App is installed only on the repos it needs
  (all `terraform-aws-*`, `dagger-*`, plus the three terragrunt repos) with
  `Contents: Read-only`. Tokens minted against that installation cannot
  reach other repos even if compromised.
- **Auditable.** Every token issuance shows up in org audit log with the
  run ID. SSH key auth and PAT auth do not.

The App ID lives in `vars.FLO_CI_APP_ID` (org variable) and the private key
lives in `secrets.FLO_CI_APP_PRIVATE_KEY` (org secret), both visible to all
repos that need them.

Inside the container we set:

```bash
git config --global \
  url."https://x-access-token:${TOKEN}@github.com/".insteadOf \
  "https://github.com/"
```

This rewrites any `https://github.com/...` clone that terragrunt issues
(including the `git::https://` sources in `.hcl`) to use the App token
transparently. The token is never written to `~/.git-credentials` — only to
an in-memory git config entry that dies with the container.

If your terragrunt repo references only public modules, omit `--git-token`
entirely. The insteadOf rule is not configured in that case, and
anonymous clones of public repos work as normal.

### IAM role trust policy sketch

The `gha-terragrunt-plan` / `gha-terragrunt-apply` roles must trust the
GitHub Actions OIDC provider. Trust policy condition on the plan role:

```json
{
  "StringEquals": {
    "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
  },
  "StringLike": {
    "token.actions.githubusercontent.com:sub": "repo:Flomenco-Inc/flo-account-admin:pull_request"
  }
}
```

Apply role narrows `sub` further to `repo:Flomenco-Inc/flo-account-admin:ref:refs/heads/main`
so only main-branch pushes can trigger an apply, never a PR branch.

Cap `MaxSessionDuration = 900` (15 min) on both roles to match the
module's default `--duration-seconds` and block callers from requesting
longer-lived sessions.

## What this module does NOT do

- **No state-file manipulation.** `terragrunt state mv`, manual backend
  changes, and disaster recovery are explicit human operations that go
  through flo-account-admin's runbooks. Automating them through Dagger
  would make footguns too easy to pull.
- **No tfsec / checkov / tflint.** Those belong in
  [`dagger-ci`](https://github.com/Flomenco-Inc/dagger-ci) and run on the
  underlying module repos at PR time. By the time code lands in a
  terragrunt repo, it's already been scanned.
- **No cross-env orchestration.** One Dagger invocation = one env. If you
  need to roll prd after stg succeeds, that's a GitHub Actions job
  dependency (`needs:`), not a Dagger function.
- **No static IAM user keys.** Ever. See credential model above.

## Versioning

Same semver discipline as `dagger-ci`:

- Patch: tool-version bump.
- Minor: new function or backwards-compatible arg addition.
- Major: breaking signature change.

Tag releases with `git tag v0.2.0 && git push --tags`.

Release history:

- `v0.1.0` — initial OIDC-native release.
- `v0.1.1` — diagnostic logging around OIDC secret mount.
- `v0.1.2` — fix AWS CLI 2.34 `--web-identity-token-file` incompatibility
  by reading the mounted OIDC secret inline into `--web-identity-token`.
- `v0.2.0` — add optional `--git-token` Secret parameter for authenticated
  cloning of private Terraform modules via a GitHub App installation
  token. Backwards-compatible for callers that only use public modules.
- `v0.3.0` — strip the v0.1.1 diagnostic logging now that the secret-mount
  path is proven stable in production.
- `v0.4.0` — add repeatable `--extra-env KEY=VALUE` flag for forwarding
  arbitrary env vars to the terragrunt exec (`get_env()` reads in HCL).
  `AWS_*` keys rejected at the boundary.
- `v0.5.0` — add optional `--leaf <subpath>` flag that scopes plan/apply
  to a single leaf under `<env>/<leaf>` (no `run --all`). Intended for
  image-only redeploys where re-initializing every other leaf is pure
  overhead. Backwards-compatible — unset `--leaf` behaves as before.
- `v0.6.0` — `plan` gets a `--summarize` flag (default `true`) that
  captures the verbose terragrunt plan output internally and emits a
  compact per-leaf changeset summary instead. Avoids GitHub Actions
  runner output size caps as the API surface grows (large
  `aws_api_gateway_rest_api.body` yamlencoded openapi diffs are the
  primary trigger). Pass `--summarize=false` to restore legacy
  verbose stream. Adds `jq` to the base container image. Apply path
  unchanged.
