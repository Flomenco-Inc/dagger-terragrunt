package baseimage

import (
	"regexp"
	"strings"
	"testing"
)

// Regression guard for Flomenco-Inc/flo#2544: an unqualified reference such
// as "debian:stable-slim" resolves to docker.io, whose anonymous pull limit
// broke every CI plan/apply.
func TestDebianAvoidsDockerHub(t *testing.T) {
	t.Parallel()

	if !strings.HasPrefix(Debian, "public.ecr.aws/docker/library/debian:") {
		t.Fatalf("base image must come from the ECR Public mirror, got %q", Debian)
	}
	if strings.Contains(Debian, "docker.io") {
		t.Fatalf("base image must not reference Docker Hub, got %q", Debian)
	}
}

func TestDebianIsDigestPinned(t *testing.T) {
	t.Parallel()

	pinned := regexp.MustCompile(`^[^@\s]+:[^@\s]+@sha256:[0-9a-f]{64}$`)
	if !pinned.MatchString(Debian) {
		t.Fatalf("base image must be pinned as name:tag@sha256:<digest>, got %q", Debian)
	}
}
