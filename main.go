// Package main exposes the Flomenco `dagger-terragrunt` Dagger module.
//
// The module wraps the two terragrunt operations that every flo terragrunt
// deployment repo (flo-account-admin, flo-core-services, flo-platform) runs
// in CI:
//
//  1. terragrunt run-all plan    → Plan   (dry-run, safe for PRs)
//  2. terragrunt run-all apply   → Apply  (writes state; gated on env approval)
//
// Plus a pre-flight Validate that does cheap structural checks (hclfmt, fmt)
// without hitting AWS.
//
// # Security model: OIDC-native, no static keys
//
// The module deliberately does NOT accept AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY. The only credential material accepted at the public
// API boundary is:
//
//   - role-arn    : the IAM role in the target AWS account
//   - oidc-token  : a short-lived GitHub-Actions-minted JWT (Secret)
//   - git-token   : OPTIONAL short-lived GitHub App installation token
//     (Secret) used to clone private Terraform modules referenced by
//     terragrunt. When omitted, only public module sources resolve.
//
// Inside the container the module:
//
//  1. Reads the OIDC JWT from the mounted secret and exchanges it via
//     `aws sts assume-role-with-web-identity` for temporary AWS session
//     credentials. AWS CLI v2 (2.34+) only accepts `--web-identity-token`
//     inline — `--web-identity-token-file` is NOT a CLI flag, only a
//     ~/.aws/config profile setting.
//  2. If a git-token was provided, configures
//     `git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/"`
//     so that terragrunt's private-module clones authenticate as the
//     GitHub App installation. The token is mounted on tmpfs and never
//     written to disk outside the git config line above.
//  3. Runs the single terragrunt invocation.
//
// Both secrets are short-lived (AWS session creds ≤15 min by default; the
// GitHub App token ≤1 h by GitHub policy), are scrubbed from Dagger logs,
// and are never persisted in the Dagger cache.
//
// Accepting `AWS_ACCESS_KEY_ID` at the module boundary would make it
// syntactically possible to pass a long-lived IAM user key. Even if every
// known caller used OIDC-minted session creds in practice, the signature IS
// the policy surface. This module eliminates the option structurally.
//
// # Module invariants
//
//   - Plan is safe to run against a real AWS account (state reads only).
//   - Apply ALWAYS requires an OIDC token + role ARN. There is no
//     "credential-free apply" code path.
//   - No cached state between runs. The container is fresh each invocation;
//     terragrunt re-downloads providers/modules.
//   - The module NEVER writes to the host filesystem beyond what is passed in
//     as a *Directory argument. The host /.aws/credentials file is never read
//     or written.
//
// # Extra env-var forwarding
//
// Both Plan and Apply take an optional repeatable `--extra-env KEY=VALUE`
// flag that forwards arbitrary env vars into the terragrunt exec. Intended
// use: terragrunt HCL can `get_env("KEY")` at parse time so a `generate`
// block can differentiate between plan and apply invocations (eg. to pick
// a read-only vs write cross-account role ARN for a provider alias).
//
// Keys starting with `AWS_` are rejected so callers cannot shadow the
// module-owned credential + region env vars. `--extra-env` is NOT a
// Secret-typed surface; sensitive material should still flow through
// `--oidc-token` / `--git-token`.
//
// # Local development
//
// Only `validate` works locally without credentials. `plan` and `apply` both
// require an OIDC token that only GitHub Actions can mint. This is
// intentional — it enforces the policy that plan/apply happen through a
// reviewed PR rather than from a developer laptop.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/dagger-terragrunt/internal/dagger"
	"dagger/dagger-terragrunt/internal/extraenv"
)

// Default tool versions. Single source of truth for Renovate bumps.
const (
	defaultTerragruntVersion = "v1.0.2"
	defaultTerraformVersion  = "1.9.8"

	// AWS region used as the default when consumers don't pass --region.
	// Matches flo's primary region. Override per-invocation if needed.
	defaultAWSRegion = "us-east-2"

	// defaultSessionName is used when the caller doesn't pass --session-name.
	// Kept generic on purpose — CloudTrail will still capture the source IP
	// + role + JWT issuer. Callers should set a CI-specific name (e.g.
	// "gha-run-<run_id>") for better traceability.
	defaultSessionName = "dagger-terragrunt"

	// defaultDurationSeconds is 15 minutes. Anything longer should require an
	// explicit override, and the IAM role's MaxSessionDuration should cap
	// this server-side.
	defaultDurationSeconds = 900

	// Where the OIDC JWT is mounted inside the container. Dagger secret
	// mounts live on a tmpfs that is only readable for the duration of the
	// exec — the right place for short-lived credential material.
	oidcTokenPath = "/run/secrets/oidc-token"

	// Where the optional GitHub App installation token is mounted. Used
	// by the insteadOf git-config rule so terragrunt's module downloads
	// authenticate as the flo-ci app installation.
	gitTokenPath = "/run/secrets/gh-token"
)

// DaggerTerragrunt is the module's root object. All exported methods are
// callable as `dagger call <method-name>` from the CLI.
type DaggerTerragrunt struct{}

// Validate runs cheap structural checks that don't need AWS credentials:
//
//   - terragrunt hclfmt --check --diff
//   - terraform fmt -check -recursive -diff   (on all *.tf under src)
//
// Intended for PR-level CI where we want fast feedback without consuming AWS
// session tokens. Also the only function that works on a dev laptop — see
// the package docstring for the "no local plan/apply" policy.
func (m *DaggerTerragrunt) Validate(
	ctx context.Context,
	// Terragrunt repo root (contains env hierarchy + root.hcl).
	src *dagger.Directory,
	// +optional
	// +default="v1.0.2"
	tgVersion string,
	// +optional
	// +default="1.9.8"
	tfVersion string,
) (string, error) {
	if tgVersion == "" {
		tgVersion = defaultTerragruntVersion
	}
	if tfVersion == "" {
		tfVersion = defaultTerraformVersion
	}

	return m.baseContainer(src, tgVersion, tfVersion).
		WithExec([]string{"sh", "-c", "set -eux; " +
			// Terragrunt v1.x split the old top-level `hclfmt` into the
			// `hcl format` subcommand. If the underlying terragrunt
			// binary is ever downgraded to v0.x, update accordingly.
			"terragrunt hcl format --check --diff && " +
			"find . -name '*.tf' -print0 | xargs -0 -r terraform fmt -check -diff"}).
		Stdout(ctx)
}

// Plan runs `terragrunt run-all plan` scoped to the given environment
// directory, authenticating to AWS via OIDC role assumption.
//
// Returns the combined stdout of the run. In CI, pipe this to a PR comment
// so reviewers see the plan inline.
func (m *DaggerTerragrunt) Plan(
	ctx context.Context,
	// Terragrunt repo root.
	src *dagger.Directory,
	// Environment directory name under the repo root, e.g. "dev", "stg",
	// "prd". Terragrunt's working dir is set to `./<env>`.
	env string,
	// ARN of the IAM role to assume via OIDC (e.g.
	// arn:aws:iam::123456789012:role/gha-terragrunt-plan). The role must
	// trust the GitHub Actions OIDC provider with a `sub` claim that
	// matches the caller's repo/branch.
	roleArn string,
	// Short-lived OIDC JWT minted by GitHub Actions for the
	// `sts.amazonaws.com` audience. Passed as a Secret so Dagger mounts it
	// on a tmpfs inside the container and never logs it.
	oidcToken *dagger.Secret,
	// Optional short-lived GitHub App installation token used to clone
	// private Terraform modules referenced from terragrunt. Mint via
	// `actions/create-github-app-token` in the caller workflow. When
	// omitted, only public module sources resolve.
	// +optional
	gitToken *dagger.Secret,
	// +optional
	// +default="us-east-2"
	region string,
	// +optional
	// +default="dagger-terragrunt"
	sessionName string,
	// +optional
	// +default=900
	durationSeconds int,
	// +optional
	// +default="v1.0.2"
	tgVersion string,
	// +optional
	// +default="1.9.8"
	tfVersion string,
	// Repeatable KEY=VALUE env-var pairs to forward into the terragrunt
	// exec. Intended for terragrunt-side `get_env()` reads — for example,
	// a generate block that picks a cross-account role ARN based on plan
	// vs apply phase can read `TG_ROLE_VARIANT` here.
	//
	// Reserved namespaces: keys starting with AWS_ are rejected. The
	// module owns the AWS_* namespace because the assume-role path sets
	// AWS_ACCESS_KEY_ID / _SECRET_ACCESS_KEY / _SESSION_TOKEN / _REGION /
	// _DEFAULT_REGION; allowing overrides would let a caller shadow those
	// and redirect the session.
	//
	// Not a Secret. Values pass through as-is to the exec's environment.
	// Sensitive values should continue to flow through --oidc-token /
	// --git-token (Secret-typed) rather than this flag.
	// +optional
	extraEnv []string,
	// Optional subpath under `--env` that scopes terragrunt to a single
	// leaf. When set, terragrunt runs plain `plan` (no `run --all`) in
	// `<env>/<leaf>`, reading dependency outputs from remote state
	// without re-planning ancestors. When empty (default), terragrunt
	// runs `run --all plan` across the full env graph as before.
	//
	// Used by image-only redeploy workflows where re-initializing every
	// other leaf in the env is pure overhead. See the flo-core-services
	// apply-webapp.yml workflow for the canonical consumer.
	//
	// Must not contain `..` or a leading `/`. Nested subpaths (eg.
	// `service-v2/subscription-service`) ARE allowed for future leaf
	// hierarchies.
	// +optional
	leaf string,
	// When true (default), the verbose terragrunt+terraform plan stdout
	// is captured to a container-side log file and a compact per-leaf
	// summary is emitted on the function's stdout instead. The summary
	// lists resource-level actions (create/update/replace/destroy) with
	// addresses but DOES NOT include attribute diff bodies.
	//
	// Why this matters: terraform's diff renderer prints the full value
	// of every changed attribute, including multi-line string values
	// like `aws_api_gateway_rest_api.body = yamlencode(<merged-openapi>)`.
	// As the API surface grows that body grows linearly, and a single
	// PR plan can balloon to multiple MB. GitHub Actions caps step output
	// (1 MB), step log streams (~4 MB), and overall job logs (~50 MB).
	// Past a few services, raw plan output starts crashing the runner
	// with "Maximum object size exceeded" before the plan even surfaces.
	//
	// On plan FAILURE, the full captured log is emitted regardless of
	// this flag — operators always need to see the failure diagnostics.
	//
	// Set --summarize=false to restore the legacy verbose stream — useful
	// when debugging an unexpected attribute diff in a small plan where
	// the verbose output is still tractable.
	// +optional
	// +default=true
	summarize bool,
) (string, error) {
	return m.runTerragrunt(
		ctx, src, env, roleArn, oidcToken, gitToken,
		region, sessionName, durationSeconds, tgVersion, tfVersion,
		extraEnv, leaf,
		"run --all plan",
		summarize,
	)
}

// Apply runs `terragrunt run-all apply --auto-approve` scoped to the given
// environment. This WILL mutate AWS state; the caller is responsible for
// gating this on a GitHub Actions environment protection rule (approvals)
// or equivalent.
//
// Same OIDC credential contract as Plan.
func (m *DaggerTerragrunt) Apply(
	ctx context.Context,
	src *dagger.Directory,
	env string,
	roleArn string,
	oidcToken *dagger.Secret,
	// Optional short-lived GitHub App installation token used to clone
	// private Terraform modules. See Plan() for details.
	// +optional
	gitToken *dagger.Secret,
	// +optional
	// +default="us-east-2"
	region string,
	// +optional
	// +default="dagger-terragrunt"
	sessionName string,
	// +optional
	// +default=900
	durationSeconds int,
	// +optional
	// +default="v1.0.2"
	tgVersion string,
	// +optional
	// +default="1.9.8"
	tfVersion string,
	// See Plan() for the extra-env contract.
	// +optional
	extraEnv []string,
	// Optional subpath under `--env` that scopes terragrunt to a single
	// leaf. When set, terragrunt runs plain `apply -- -auto-approve`
	// (no `run --all`) in `<env>/<leaf>`. See Plan() for the full
	// rationale + path constraints.
	//
	// Dependencies are NOT re-applied; their outputs are read from
	// remote state. This mode assumes the full env has been applied at
	// least once via run-all; using --leaf against an unbuilt env will
	// fail at dependency resolution.
	// +optional
	leaf string,
) (string, error) {
	return m.runTerragrunt(
		ctx, src, env, roleArn, oidcToken, gitToken,
		region, sessionName, durationSeconds, tgVersion, tfVersion,
		extraEnv, leaf,
		// --auto-approve is a Terraform flag, not a Terragrunt flag;
		// Terragrunt >=0.68 requires forwarding such flags after `--`.
		// The gate for this mutation is the GHA environment approval
		// upstream — the human ok happens before this container ever
		// runs. If that gate is removed, --auto-approve must be removed
		// too or nothing gates.
		"run --all apply -- -auto-approve",
		// summarize=false on apply: the per-resource progress messages
		// terraform prints during apply are operationally useful (when
		// did each resource start, did it block, did it retry?) and
		// they don't carry the attribute-diff bodies that bloat plan
		// output. If apply output ever does grow past runner caps, we
		// can revisit by parsing terraform's apply -json output stream.
		false,
	)
}

// ---------------------------------------------------------------------------
// Internal helpers — not exposed as Dagger functions.
// ---------------------------------------------------------------------------

// runTerragrunt is the shared implementation behind Plan and Apply. It:
//
//  1. Normalises defaults.
//  2. Mounts the OIDC token as a read-only secret file at /run/secrets/oidc-token.
//  3. Runs a single shell invocation that exchanges the OIDC token for
//     temporary session credentials via `aws sts assume-role-with-web-identity`,
//     exports them as env vars in that shell, and then runs terragrunt.
//
// The assume-role output is captured via `--output text --query` to avoid a
// jq dependency. We read the three credential fields with `read`, export
// them, and never write them to disk.
func (m *DaggerTerragrunt) runTerragrunt(
	ctx context.Context,
	src *dagger.Directory,
	env string,
	roleArn string,
	oidcToken *dagger.Secret,
	gitToken *dagger.Secret,
	region string,
	sessionName string,
	durationSeconds int,
	tgVersion string,
	tfVersion string,
	extraEnv []string,
	leaf string,
	terragruntCmd string,
	summarize bool,
) (string, error) {
	extraEnvMap, err := extraenv.Parse(extraEnv)
	if err != nil {
		return "", err
	}
	if region == "" {
		region = defaultAWSRegion
	}
	if sessionName == "" {
		sessionName = defaultSessionName
	}
	if durationSeconds == 0 {
		durationSeconds = defaultDurationSeconds
	}
	if tgVersion == "" {
		tgVersion = defaultTerragruntVersion
	}
	if tfVersion == "" {
		tfVersion = defaultTerraformVersion
	}

	if roleArn == "" {
		return "", fmt.Errorf("role-arn is required")
	}
	if oidcToken == nil {
		return "", fmt.Errorf("oidc-token is required")
	}
	if env == "" {
		return "", fmt.Errorf("env is required")
	}

	// When --leaf is set, scope terragrunt to a single leaf dir (no
	// run-all). This is the image-only-redeploy optimization: reading
	// an env's full DAG costs ~60-90s of init on every re-apply even
	// when only one leaf's state could have changed. With --leaf,
	// terragrunt only initializes the named leaf and reads its deps'
	// outputs from remote state.
	//
	// Guardrails on the subpath: no `..` (prevents traversing out of
	// the env), no leading `/` (prevents absolute paths escaping the
	// source mount). Nested relative subpaths are allowed for forward
	// compatibility with multi-leaf service hierarchies (eg.
	// service-v2/subscription-service).
	cdPath := "./" + env
	if leaf != "" {
		if strings.Contains(leaf, "..") {
			return "", fmt.Errorf("leaf must not contain '..': got %q", leaf)
		}
		if strings.HasPrefix(leaf, "/") {
			return "", fmt.Errorf("leaf must be a relative subpath, not absolute: got %q", leaf)
		}
		cdPath = "./" + env + "/" + leaf
		// Strip the run-all prefix so we get `terragrunt plan` /
		// `terragrunt apply -- -auto-approve` in a single dir.
		terragruntCmd = strings.TrimPrefix(terragruntCmd, "run --all ")
	}

	// Build the bootstrap shell script that runs assume-role-with-web-identity
	// and then execs terragrunt. We carefully avoid `set -x` because we do
	// NOT want the session credentials echoed to stdout. `set -e` is enough
	// to fail fast on any error.
	// AWS CLI v2 (2.34+) only exposes `--web-identity-token <value>` as a
	// CLI flag; `--web-identity-token-file` is NOT a valid CLI parameter
	// (it only exists as a `web_identity_token_file` profile setting in
	// ~/.aws/config). We therefore read the mounted secret file once into
	// a local shell variable and pass it inline. The variable lives only
	// for the duration of the shell script and is never exported or
	// logged.
	//
	// If a gitToken was provided, we configure an insteadOf rule so that
	// any `https://github.com/...` clone (including the ones terragrunt
	// issues to fetch private Terraform module sources) is rewritten to
	// authenticate with the GitHub App installation token. The token is
	// embedded in git's in-memory config only; it is not written to a
	// credential file and is not logged (Dagger scrubs Secret values).
	gitAuthBlock := ""
	if gitToken != nil {
		gitAuthBlock = fmt.Sprintf(`gh_token=$(cat %q)
git config --global url."https://x-access-token:${gh_token}@github.com/".insteadOf "https://github.com/"
unset gh_token
`, gitTokenPath)
	}

	// When summarizing a plan we splice `-out=tfplan.bin` onto the
	// terragrunt command and post-process. terragrunt forwards trailing
	// flags after `--` to terraform; the existing apply path uses the
	// same convention with `-auto-approve`.
	terragruntCmdEffective := terragruntCmd
	tgCmdBlock := fmt.Sprintf("terragrunt --non-interactive %s\n", terragruntCmdEffective)
	if summarize {
		// Inject -out=tfplan.bin BEFORE any existing `--` so we end up
		// with eg. `run --all plan -- -out=tfplan.bin` regardless of
		// run-all vs leaf-scoped vs whatever caller arg shape.
		if strings.Contains(terragruntCmdEffective, " -- ") {
			terragruntCmdEffective = strings.Replace(
				terragruntCmdEffective, " -- ", " -- -out=tfplan.bin ", 1)
		} else {
			terragruntCmdEffective = terragruntCmdEffective + " -- -out=tfplan.bin"
		}
		// On success, walk every plan binary terragrunt produced and
		// emit a per-leaf compact summary via `terraform show -json`.
		// The full verbose plan stays in /tmp/plan.log inside the
		// container — invisible from outside, which is the goal here.
		// On failure we cat the full log so operators see diagnostics.
		//
		// Why per-leaf walking instead of `terragrunt run-all show`:
		// run-all show would re-emit the same verbose textual plan we
		// just suppressed. We need the JSON form, which run-all does
		// not pass through cleanly. Walking the cache dirs and calling
		// terraform directly gets us deterministic JSON with no extra
		// terragrunt overhead.
		// Bash script structure:
		//   1. run terragrunt plan, redirecting both streams to a tmp
		//      log file so the verbose body diffs don't reach stdout
		//   2. on plan failure, cat the log + exit 1
		//   3. on success, walk every tfplan.bin under cwd, run
		//      `terraform show -no-color -json tfplan.bin` per-leaf,
		//      emit a per-leaf header + counts + per-resource action
		//      lines, then totals across all leaves
		//
		// jq filters:
		//   - per-resource line: skip no-op + read-only actions
		//   - replace count: an action list that includes both "create"
		//     AND "delete" (terraform plans replacements as
		//     ["delete","create"] or ["create","delete"] depending on
		//     create_before_destroy)
		//
		// Subshell-from-pipe trap: `find ... | while read` runs the
		// while body in a subshell, so accumulator variables don't
		// survive. We avoid that by writing the find output to a tmp
		// file and looping with `< "$plans_list"`.
		tgCmdBlock = fmt.Sprintf(`plan_log=$(mktemp)
plans_list=$(mktemp)
trap 'rm -f "$plan_log" "$plans_list"' EXIT
if ! terragrunt --non-interactive %s > "$plan_log" 2>&1; then
  echo "::error::terragrunt plan failed; full log follows" >&2
  cat "$plan_log"
  exit 1
fi

find . -name tfplan.bin -type f | sort > "$plans_list"
if ! [ -s "$plans_list" ]; then
  echo "=== Plan summary ==="
  echo "(no plan files produced — nothing to apply)"
  exit 0
fi

leaf_count=$(wc -l < "$plans_list" | tr -d ' ')
echo "=== Plan summary across $leaf_count leaves (pass --summarize=false for full diff) ==="
echo

while IFS= read -r p; do
  # Strip leading ./ then everything from /.terragrunt-cache/ onward
  # to get a stable per-leaf label like envs/dev/us-east-1/auth.
  leaf_label=$(echo "$p" | sed -E 's|^\./||; s|/\.terragrunt-cache/.*||')
  json=$(cd "$(dirname "$p")" && terraform show -no-color -json tfplan.bin)
  echo "--- $leaf_label"
  echo "$json" | jq -r '
    (.resource_changes // []) as $rc
    | ($rc | map(select(.change.actions == ["create"])) | length) as $c
    | ($rc | map(select(.change.actions == ["update"])) | length) as $u
    | ($rc | map(select(.change.actions | (contains(["delete"]) and contains(["create"]))))
            | length) as $r
    | ($rc | map(select(.change.actions == ["delete"])) | length) as $d
    | "  " + ($c|tostring) + " create / " + ($u|tostring) + " update / "
            + ($r|tostring) + " replace / " + ($d|tostring) + " destroy",
      ($rc[]
       | select(.change.actions != ["no-op"])
       | select(.change.actions != ["read"])
       | "    " + (.change.actions | join("+")) + "  " + .address)'
done < "$plans_list"

# Totals: re-walk plans_list and concatenate JSON, then a single jq
# pass aggregates across leaves. Cheap (each plan binary is small)
# and avoids accumulator-state-in-subshell pitfalls.
echo
echo "=== Totals ==="
while IFS= read -r p; do
  ( cd "$(dirname "$p")" && terraform show -no-color -json tfplan.bin )
done < "$plans_list" | jq -s -r '
  [.[].resource_changes[]?] as $all
  | "  create:  " + ([$all[] | select(.change.actions == ["create"])] | length | tostring),
    "  update:  " + ([$all[] | select(.change.actions == ["update"])] | length | tostring),
    "  replace: " + ([$all[] | select(.change.actions
                                     | (contains(["delete"]) and contains(["create"])))]
                              | length | tostring),
    "  destroy: " + ([$all[] | select(.change.actions == ["delete"])] | length | tostring)'
`, terragruntCmdEffective)
	}

	script := fmt.Sprintf(`set -eu
oidc_jwt=$(cat %q)
creds=$(aws sts assume-role-with-web-identity \
  --role-arn %q \
  --role-session-name %q \
  --web-identity-token "$oidc_jwt" \
  --duration-seconds %d \
  --output text \
  --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]')
unset oidc_jwt

# $creds is a tab-separated triple on one line. Parse into env vars.
IFS=$'\t' read -r AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN <<EOF
$creds
EOF
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
unset creds

%s
cd %q
%s`,
		oidcTokenPath,
		roleArn, sessionName, durationSeconds,
		gitAuthBlock,
		cdPath, tgCmdBlock,
	)

	c := m.baseContainer(src, tgVersion, tfVersion).
		WithEnvVariable("AWS_REGION", region).
		WithEnvVariable("AWS_DEFAULT_REGION", region)

	// Caller-supplied env vars. Applied AFTER the AWS_* envs so a
	// mis-authored caller cannot accidentally shadow them — extraenv.Parse
	// rejects AWS_* keys but keeping the application order defensive
	// too is cheap and makes the invariant visible.
	for k, v := range extraEnvMap {
		c = c.WithEnvVariable(k, v)
	}

	c = c.
		// AWS CLI v2 is only needed for plan/apply (not validate), so we
		// install it here rather than in baseContainer. Keeps validate
		// lean and avoids pulling a ~100MB bundle into a code path that
		// never touches AWS. Also makes validate runnable on non-amd64
		// hosts without qemu emulation quirks.
		WithExec([]string{"sh", "-c", "set -eu; " +
			"arch=$(uname -m); " +
			"curl -fsSLo /tmp/awscli.zip " +
			"https://awscli.amazonaws.com/awscli-exe-linux-${arch}.zip && " +
			"unzip -q /tmp/awscli.zip -d /tmp && " +
			"/tmp/aws/install -i /usr/local/aws -b /usr/local/bin && " +
			"rm -rf /tmp/awscli.zip /tmp/aws && aws --version"}).
		// Mount OIDC JWT as a tmpfs file. Dagger guarantees the file is
		// only readable for the lifetime of the exec and is never cached.
		WithMountedSecret(oidcTokenPath, oidcToken)

	if gitToken != nil {
		c = c.WithMountedSecret(gitTokenPath, gitToken)
	}

	return c.
		// bash, not sh. The bootstrap script uses $'\t' for tab-delimiter
		// IFS splitting. Running under dash (Debian's default /bin/sh)
		// silently produces garbage AWS session credentials because the
		// $'\t' escape is interpreted literally, so the whole credential
		// triple lands in AWS_ACCESS_KEY_ID and every signed API call
		// fails with IncompleteSignature. Pin bash.
		WithExec([]string{"bash", "-c", script}).
		Stdout(ctx)
}

// baseContainer returns a container with terraform + terragrunt + git +
// ca-certificates installed and the source mounted at /src.
//
// Deliberately does NOT install AWS CLI — that's handled in runTerragrunt
// because validate doesn't need it and we want validate to stay fast and
// host-arch-agnostic.
//
// Base: debian:stable-slim. HashiCorp + Gruntwork binaries are glibc-linked.
// Tool downloads use architecture-detected URLs so the container runs
// natively on both amd64 (CI) and arm64 (Apple Silicon dev laptops).
func (m *DaggerTerragrunt) baseContainer(
	src *dagger.Directory,
	tgVersion, tfVersion string,
) *dagger.Container {
	// uname -m returns x86_64 / aarch64; HashiCorp + Gruntwork use
	// amd64 / arm64 in their release URLs. Normalise once in shell and
	// reuse via the $tfarch variable.
	archNormalise := `arch=$(uname -m); case "$arch" in x86_64) tfarch=amd64 ;; aarch64) tfarch=arm64 ;; *) echo "unsupported arch: $arch" >&2; exit 1 ;; esac`

	return dag.Container().
		From("debian:stable-slim").
		// bash is explicitly installed alongside the usual tooling. The
		// terragrunt bootstrap script relies on bash-only features
		// (`$'\t'` ANSI-C quoting for IFS, `${VAR:0:N}` substring
		// expansion) which dash rejects with "Bad substitution". We could
		// rewrite in strict POSIX sh, but the script is already complex
		// enough that depending on bash is cheaper and clearer.
		WithExec([]string{"sh", "-c", "set -eux; " +
			"apt-get update && apt-get install -y --no-install-recommends " +
			// jq is used by the plan summarize path in runTerragrunt to
			// turn `terraform show -json tfplan.bin` into a compact
			// per-leaf changeset list. Cheap (~600 KB), pinned via
			// debian:stable-slim's package set.
			"ca-certificates curl unzip git bash jq && rm -rf /var/lib/apt/lists/*"}).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"set -eux; %s; "+
				"curl -fsSLo /tmp/tf.zip "+
				"https://releases.hashicorp.com/terraform/%[2]s/terraform_%[2]s_linux_${tfarch}.zip && "+
				"unzip -q /tmp/tf.zip -d /usr/local/bin && "+
				"rm /tmp/tf.zip && terraform version",
			archNormalise, tfVersion,
		)}).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"set -eux; %s; "+
				"curl -fsSLo /usr/local/bin/terragrunt "+
				"https://github.com/gruntwork-io/terragrunt/releases/download/%[2]s/terragrunt_linux_${tfarch} && "+
				"chmod +x /usr/local/bin/terragrunt && terragrunt --version",
			archNormalise, tgVersion,
		)}).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src")
}
