package storage

import (
	"errors"
	"testing"

	"github.com/aws/smithy-go"
)

func TestCleanKey(t *testing.T) {
	s := &Store{prefix: "releases"}
	k, err := s.cleanKey("group/artifact/1.0/app.jar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k != "releases/group/artifact/1.0/app.jar" {
		t.Fatalf("got %s", k)
	}
	if _, err := s.cleanKey(""); err == nil {
		t.Fatalf("expected error on empty key")
	}
	if k, err := s.cleanKey("../etc/passwd"); err != nil || k != "releases/etc/passwd" {
		t.Fatalf("expected sanitized path, got key=%s err=%v", k, err)
	}
}

func TestIsNotFound(t *testing.T) {
	apiErr := smithy.GenericAPIError{Code: "NotFound"}
	if !IsNotFound(&apiErr) {
		t.Fatalf("expected not found for api error")
	}
	if IsNotFound(errors.New("other")) {
		t.Fatalf("did not expect other error to be not found")
	}
}
