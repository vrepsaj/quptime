package daemon

import (
	"testing"
	"time"
)

func TestHashEnrollSecretIsStable(t *testing.T) {
	got := hashEnrollSecret("hello-world")
	want := hashEnrollSecret("hello-world")
	if got != want {
		t.Fatalf("hash is not deterministic: %q vs %q", got, want)
	}
	if len(got) < len("sha256:") || got[:7] != "sha256:" {
		t.Errorf("hash missing sha256: prefix: %q", got)
	}
}

func TestVerifyEnrollSecret(t *testing.T) {
	secret := "supersecret"
	stored := hashEnrollSecret(secret)
	if !verifyEnrollSecret(secret, stored) {
		t.Error("correct secret rejected")
	}
	if verifyEnrollSecret("wrong", stored) {
		t.Error("wrong secret accepted")
	}
	if verifyEnrollSecret(secret, "sha256:00") {
		t.Error("bogus stored-hash accepted")
	}
	if verifyEnrollSecret(secret, "not-prefixed") {
		t.Error("unprefixed stored-hash accepted")
	}
}

func TestEnrollmentTokenRoundTrip(t *testing.T) {
	in := EnrollmentTokenPayload{
		ID:     "tok-1",
		Secret: "s3cret",
		Endpoints: []EnrollEndpoint{
			{Advertise: "alpha.example.com:9901", Fingerprint: "sha256:abc"},
			{Advertise: "bravo.example.com:9901", Fingerprint: "sha256:def"},
		},
		ExpiresAt: time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second),
	}
	encoded := EncodeEnrollmentToken(in)
	out, err := DecodeEnrollmentToken(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Secret != in.Secret {
		t.Errorf("id/secret mismatch after round-trip: %+v", out)
	}
	if len(out.Endpoints) != 2 || out.Endpoints[0].Advertise != "alpha.example.com:9901" {
		t.Errorf("endpoints lost in round-trip: %+v", out.Endpoints)
	}
	if !out.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("expiry drifted: %v vs %v", out.ExpiresAt, in.ExpiresAt)
	}
}

func TestDecodeEnrollmentTokenRejectsGarbage(t *testing.T) {
	if _, err := DecodeEnrollmentToken("not-base64-!!"); err == nil {
		t.Error("invalid base64 accepted")
	}
	// Valid base64, invalid JSON.
	if _, err := DecodeEnrollmentToken("YWJj"); err == nil {
		t.Error("non-JSON payload accepted")
	}
	// Valid JSON, missing required fields.
	empty := EncodeEnrollmentToken(EnrollmentTokenPayload{})
	if _, err := DecodeEnrollmentToken(empty); err == nil {
		t.Error("empty payload accepted")
	}
}
