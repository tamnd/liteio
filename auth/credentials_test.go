// SPDX-License-Identifier: Apache-2.0

package auth

import "testing"

func TestStaticStoreGet(t *testing.T) {
	root := Credentials{AccessKey: "AK1", SecretKey: "S1"}
	other := Credentials{AccessKey: "AK2", SecretKey: "S2"}
	store := NewStaticStore(root, other)

	got, ok := store.Get("AK1")
	if !ok || got != root {
		t.Fatalf("Get(AK1) = %+v, %v; want %+v, true", got, ok, root)
	}
	got, ok = store.Get("AK2")
	if !ok || got != other {
		t.Fatalf("Get(AK2) = %+v, %v; want %+v, true", got, ok, other)
	}
	if _, ok := store.Get("missing"); ok {
		t.Fatal("Get(missing) should report not found")
	}
	if _, ok := store.Get(""); ok {
		t.Fatal("Get(empty) should report not found")
	}
}

func TestStaticStoreEmpty(t *testing.T) {
	store := NewStaticStore()
	if _, ok := store.Get("anything"); ok {
		t.Fatal("empty store should never hit")
	}
}
