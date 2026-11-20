package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BurntSushi/toml"
)

// The generated template must parse back to exactly the bindings it was
// generated from, defaults and user overrides alike.
func TestKeymapTOMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "wtui"), 0o755); err != nil {
		t.Fatal(err)
	}
	override := "[browse]\nsearchChats = [\"c\", \"ctrl+/\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "wtui", "keys.toml"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	k := loadKeymap()

	var parsed map[string]map[string][]string
	if err := toml.Unmarshal([]byte(keymapTOML(k)), &parsed); err != nil {
		t.Fatalf("generated TOML doesn't parse: %v", err)
	}
	if !maps.EqualFunc(parsed, k.byAction, func(a, b map[string][]string) bool {
		return maps.EqualFunc(a, b, slices.Equal)
	}) {
		t.Errorf("round trip mismatch:\n got %v\nwant %v", parsed, k.byAction)
	}
	if got := parsed["browse"]["searchChats"]; !slices.Equal(got, []string{"c", "ctrl+/"}) {
		t.Errorf("override lost: searchChats = %v", got)
	}
}
