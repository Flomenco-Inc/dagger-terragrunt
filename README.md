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

Inside the Dagger container the module runs
`aws sts assume-role-with-web-identity` and exports the resulting session
credentials as env vars **for the single terragrunt invocation only**. The
session creds never leave the Dagger exec, and the OIDC token is mounted on
a tmpfs (`/run/secrets/oidc-token`) so it is never logged, never persisted
in the Dagger cache, and never exposed as a container env var.

No path accepts `--aws-access-key-id` / `--aws-secret-access-key`. That
would make it syntactically possible to pass a long-lived IAM user key, and
even if every known caller used OIDC-minted session creds in practice, the
signature IS the policy surface. We eliminated the option structurally.

## Functions

| Function    | Credentials              | Mutates state?        | When to call it                          |
| ----------- | ------------------------ | --------------------- | ---------------------------------------- |
| `validate`  | **none**                 | no                    | Every PR — fast structural checks.       |
| `plan`      | OIDC role + token        | no (read-only APIs)   | Every PR — shows diff, posts to PR body. |
| `apply`     | OIDC role + token        | **YES**               | After PR merge + environment approval.   |

### `validate --src=<dir> [--tg-version=v1.0.2] [--tf-version=1.9.8]`

Runs:

- `terragrunt hcl format --check --diff`
- `terraform fmt -check -recursive -diff` across every `.tf` under the tree

No AWS calls. No credentials required. Safe to run on untrusted PRs.

### `plan --src=<dir> --env=<dev|stg|prd> --role-arn=<arn> --oidc-token=env://OIDC_TOKEN [opts]`

Runs `terragrunt --non-interactive run --all plan` scoped to `./<env>/`.

Required args:

- `--src` — terragrunt repo root directory.
- `--env` — env directory name under the repo root (`dev`, `stg`, `prd`).
- `--role-arn` — IAM role ARN to assume. The role must trust the GitHub
  Actions OIDC provider with a `sub` claim matching the caller's repo/branch.
- `--oidc-token` — OIDC JWT minted by `core.getIDToken("sts.amazonaws.com")`.
  Always pass as a Dagger secret (`env://OIDC_TOKEN`) so the plaintext never
  touches the command line.

Optional:

- `--region` (default `us-east-2`)
- `--session-name` (default `dagger-terragrunt`; set to something
  CI-specific like `gha-${{ github.run_id }}` for traceability in CloudTrail)
- `--duration-seconds` (default `900` — matches the intended
  `MaxSessionDuration` on the plan/apply roles; must be ≤ the role's cap)
- `--tg-version`, `--tf-version`

### `apply --src=<dir> --env=<...> --role-arn=<arn> --oidc-token=env://OIDC_TOKEN [opts]`

Same signature as `plan`. Runs
`terragrunt --non-interactive run --all apply --auto-approve`. **This
mutates AWS state.** Gate it on a GitHub Actions environment protection rule
with required reviewers (see CI integration below).

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
        id: oidc
        uses: actions/github-script@v7
        with:
          script: |
            const tok = await core.getIDToken("sts.amazonaws.com");
            core.setSecret(tok);
            core.exportVariable("OIDC_TOKEN", tok);

      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: |
            call -m github.com/Flomenco-Inc/dagger-terragrunt@v0.1.0 \
              plan --src=. --env=${{ matrix.env }} \
                --role-arn=arn:aws:iam::${{ vars.FLO_DEV_ACCOUNT_ID }}:role/gha-terragrunt-plan \
                --oidc-token=env://OIDC_TOKEN \
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
        id: oidc
        uses: actions/github-script@v7
        with:
          script: |
            const tok = await core.getIDToken("sts.amazonaws.com");
            core.setSecret(tok);
            core.exportVariable("OIDC_TOKEN", tok);

      - uses: dagger/dagger-for-github@v8
        with:
          version: v0.20.6
          call: |
            call -m github.com/Flomenco-Inc/dagger-terragrunt@v0.1.0 \
              apply --src=. --env=${{ matrix.env }} \
                --role-arn=arn:aws:iam::${{ vars.FLO_DEV_ACCOUNT_ID }}:role/gha-terragrunt-apply \
                --oidc-token=env://OIDC_TOKEN \
                --session-name=gha-${{ github.run_id }}
```

The required-reviewers gate sits on the GitHub Actions **environment**
(`dev` / `stg` / `prd`), not on the branch. That's how we enforce the
"two-approver rule for prd applies" from the migration plan. Branch
protection only enforces that commits landed on `main` via PR — approval
to **deploy** is a separate, per-env decision.

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

Tag releases with `git tag v0.1.0 && git push --tags`. No v0.1.0-prior
history exists — the OIDC-native design is the initial public release.
