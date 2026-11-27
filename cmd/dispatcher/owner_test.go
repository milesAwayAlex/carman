package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"carman/internal/dispatch/api"
)

// TestR2OwnerHashSetting: the service takes the owner's token only as its
// hash (ADR 0051), and refuses to start with the token in the clear.
func TestR2OwnerHashSetting(t *testing.T) {
	sum := sha256.Sum256([]byte("t"))
	got, err := ownerHash(map[string]api.Role{"a": api.RoleAgent}, hex.EncodeToString(sum[:])+"\n")
	if err != nil || string(got) != string(sum[:]) {
		t.Errorf("a hash: %x, %v", got, err)
	}
	if got, err := ownerHash(map[string]api.Role{"a": api.RoleAgent}, ""); err != nil || got != nil {
		t.Errorf("no hash: %x, %v; want no owner and no error", got, err)
	}
	if _, err := ownerHash(map[string]api.Role{"o": api.RoleOwner}, hex.EncodeToString(sum[:])); err == nil {
		t.Error("an owner token in DISPATCHER_TOKENS was accepted")
	}
	for _, bad := range []string{"abc", hex.EncodeToString(sum[:16]), "zz" + hex.EncodeToString(sum[1:])} {
		if _, err := ownerHash(nil, bad); err == nil {
			t.Errorf("%q was taken for a hash", bad)
		}
	}
}
