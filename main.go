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
//
// Inside the container the module runs
//
//	aws sts assume-role-with-web-identity --role-arn … --web-identity-token-file /run/secrets/oidc-token
//
// and exports the returned session credentials as env vars for the single
// terragrunt invocation. The session creds never leave the Dagger exec, and
// the OIDC token is only readable inside the container via the tmpfs secret
// mount — it is not logged, not persisted in the Dagger cache, and not
// exposed as a container env var.
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

	"dagger/dagger-terragrunt/internal/dagger"
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
) (string, error) {
	return m.runTerragrunt(
		ctx, src, env, roleArn, oidcToken,
		region, sessionName, durationSeconds, tgVersion, tfVersion,
		"run --all plan",
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
) (string, error) {
	return m.runTerragrunt(
		ctx, src, env, roleArn, oidcToken,
		region, sessionName, durationSeconds, tgVersion, tfVersion,
		// --auto-approve because apply is gated at the GHA environment
		// level — the human approval happens before this container ever
		// runs. If the environment gate is removed, --auto-approve must
		// be removed too or nothing gates.
		"run --all apply --auto-approve",
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
	region string,
	sessionName string,
	durationSeconds int,
	tgVersion string,
	tfVersion string,
	terragruntCmd string,
) (string, error) {
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

	// Build the bootstrap shell script that runs assume-role-with-web-identity
	// and then execs terragrunt. We carefully avoid `set -x` because we do
	// NOT want the session credentials echoed to stdout. `set -e` is enough
	// to fail fast on any error.
	script := fmt.Sprintf(`set -eu
# Diagnostic: confirm the OIDC token is actually mounted and non-empty.
# Only file size is logged; the plaintext never appears in output.
if [ -f %q ]; then
  echo "DEBUG: oidc token file size (bytes): $(wc -c < %q)"
else
  echo "DEBUG: oidc token file MISSING at %s"
  ls -la %s 2>&1 || true
fi
creds=$(aws sts assume-role-with-web-identity \
  --role-arn %q \
  --role-session-name %q \
  --web-identity-token-file %q \
  --duration-seconds %d \
  --output text \
  --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]')

# $creds is a tab-separated triple on one line. Parse into env vars.
IFS=$'\t' read -r AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN <<EOF
$creds
EOF
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
unset creds

cd %q
terragrunt --non-interactive %s
`,
		oidcTokenPath, oidcTokenPath, oidcTokenPath,
		// `dirname` of the path, for the `ls -la` diagnostic when the file
		// is missing. Hardcoded because the tmpfs mount always lives here.
		"/run/secrets",
		roleArn, sessionName, oidcTokenPath, durationSeconds,
		"./"+env, terragruntCmd,
	)

	return m.baseContainer(src, tgVersion, tfVersion).
		WithEnvVariable("AWS_REGION", region).
		WithEnvVariable("AWS_DEFAULT_REGION", region).
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
		WithMountedSecret(oidcTokenPath, oidcToken).
		WithExec([]string{"sh", "-c", script}).
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
		WithExec([]string{"sh", "-c", "set -eux; " +
			"apt-get update && apt-get install -y --no-install-recommends " +
			"ca-certificates curl unzip git && rm -rf /var/lib/apt/lists/*"}).
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
