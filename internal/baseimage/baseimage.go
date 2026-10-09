// Package baseimage pins the container base image used by every
// dagger-terragrunt function.
//
// The image is Docker's official debian:stable-slim, pulled from the ECR
// Public mirror of Docker Official Images (public.ecr.aws/docker/library)
// rather than Docker Hub. GitHub-hosted runners pull anonymously, and on
// 2026-10-09 Docker Hub's unauthenticated rate limit plus auth-endpoint
// timeouts failed every plan/apply with
// `failed to resolve image "docker.io/library/debian:stable-slim"`
// (Flomenco-Inc/flo#2544). The ECR Public mirror serves the same OCI index
// digest as Docker Hub for this tag.
//
// The digest pin keeps runs reproducible; Renovate bumps it (see the
// customManagers entry in renovate.json).
package baseimage

// Debian is the fully qualified, digest-pinned base image reference.
const Debian = "public.ecr.aws/docker/library/debian:stable-slim@sha256:eb593cf2c358cacef45ca0a424bbc7d30cfa3466265fc2662b9466a0ca6ba1c5"
