package s3

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/digital-shokunin/kura/internal/store"
	"github.com/digital-shokunin/kura/internal/store/storetest"
)

// isolate keeps the developer's AWS configuration out of the test.
func isolate(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
}

func open(t *testing.T, raw string) (*Store, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return Open(context.Background(), u)
}

func TestOpenValidates(t *testing.T) {
	isolate(t)
	if _, err := open(t, "s3:///prefix?region=us-east-2"); err == nil {
		t.Error("Open accepted a URL without a bucket")
	}
	if _, err := open(t, "s3://bucket/prefix"); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("Open without a region: %v", err)
	}
	for raw, prefix := range map[string]string{
		"s3://bucket?region=us-east-2":          "",
		"s3://bucket/?region=us-east-2":         "",
		"s3://bucket/a/b/?region=us-east-2":     "a/b/",
		"s3://bucket/repo?region=us-east-2&x=1": "repo/",
	} {
		s, err := open(t, raw)
		if err != nil {
			t.Fatalf("Open(%q): %v", raw, err)
		}
		if s.bucket != "bucket" || s.prefix != prefix {
			t.Errorf("Open(%q) = bucket %q prefix %q, want prefix %q", raw, s.bucket, s.prefix, prefix)
		}
	}
}

func TestOpenNamedCredentials(t *testing.T) {
	isolate(t)
	defer func(old func(string) (string, string, error)) { CredentialLookup = old }(CredentialLookup)
	var asked string
	CredentialLookup = func(name string) (string, string, error) {
		asked = name
		if name == "missing" {
			return "", "", errors.New("not stored")
		}
		return "AKIDEXAMPLE", "secret", nil
	}
	s, err := open(t, "s3://bucket/p?region=us-east-2&creds=work")
	if err != nil || asked != "work" {
		t.Fatalf("Open with creds=work: %v (looked up %q)", err, asked)
	}
	got, err := s.client.Options().Credentials.Retrieve(context.Background())
	if err != nil || got.AccessKeyID != "AKIDEXAMPLE" {
		t.Fatalf("credentials = %+v, %v", got, err)
	}
	if _, err := open(t, "s3://bucket/p?region=us-east-2&creds=missing"); err == nil {
		t.Fatal("Open ignored a failed credential lookup")
	}
}

// TestConformance runs the shared store checks, including the put-if-absent
// race, against a live S3-compatible endpoint. CI points it at versitygw:
//
//	KURA_TEST_S3_URL='s3://bucket/prefix?region=us-east-1&endpoint=http://127.0.0.1:7070&path_style=true'
//
// with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY set.
func TestConformance(t *testing.T) {
	raw := os.Getenv("KURA_TEST_S3_URL")
	if raw == "" {
		t.Skip("KURA_TEST_S3_URL not set")
	}
	s, err := open(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range storetest.Checks() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Run(context.Background(), s, "t/"); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("head_missing_returns_ErrNotFound", func(t *testing.T) {
		// Manifest probing depends on this; a 403 here means missing ListBucket.
		if _, err := s.Head(context.Background(), "t/definitely-missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Head of a missing object: %v, want store.ErrNotFound", err)
		}
	})
}
