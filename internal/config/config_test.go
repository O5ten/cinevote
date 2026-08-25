package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Nothing set: the app has to be runnable out of the box.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load with an empty environment: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.DBPath != DefaultDBPath || cfg.MaxVotes != 5 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.Demo || cfg.FreshDB {
		t.Error("demo mode must never be on by default")
	}
}

func TestLoadRejectsNonsense(t *testing.T) {
	t.Setenv("CINEVOTE_MAX_VOTES", "not-a-number")
	if _, err := Load(); err == nil {
		t.Error("a non-numeric vote count should fail at startup, not silently")
	}

	t.Setenv("CINEVOTE_MAX_VOTES", "0")
	if _, err := Load(); err == nil {
		t.Error("zero votes per user should be rejected")
	}
}

func TestDemoFromEnvironment(t *testing.T) {
	t.Setenv("CINEVOTE_DEMO", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Demo {
		t.Fatal("CINEVOTE_DEMO=true should turn demo mode on")
	}
}

// The whole point of demo mode: nothing needs to be configured, and it picks a
// throwaway database it is allowed to wipe.
func TestApplyDemoDefaultsNeedsNoConfiguration(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if replaced := cfg.ApplyDemoDefaults("demo-password"); replaced != "" {
		t.Errorf("nothing was configured, so nothing should be reported as replaced, got %q", replaced)
	}

	if !cfg.Demo {
		t.Error("demo flag not set")
	}
	if cfg.DBPath != DemoDBPath() {
		t.Errorf("db path = %q, want the throwaway path %q", cfg.DBPath, DemoDBPath())
	}
	if !cfg.FreshDB {
		t.Error("a demo on its own throwaway database should be recreated on start")
	}
	if cfg.AdminPassword != "demo-password" {
		t.Errorf("admin password = %q, want the demo password", cfg.AdminPassword)
	}
	if cfg.RegistrationCode != "" {
		t.Error("an invite code would defeat a click-and-try demo")
	}
}

// Demo mode always uses its own throwaway database, so it can reseed freely and
// can never delete or seed into a real one.
func TestApplyDemoDefaultsAlwaysUsesTheThrowawayDatabase(t *testing.T) {
	t.Setenv("CINEVOTE_DB", "/srv/cinevote/real.db")
	t.Setenv("CINEVOTE_ADMIN_PASSWORD", "chosen-by-a-human")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	replaced := cfg.ApplyDemoDefaults("demo-password")

	if cfg.DBPath != DemoDBPath() {
		t.Errorf("db path = %q, want the throwaway path %q", cfg.DBPath, DemoDBPath())
	}
	if !cfg.FreshDB {
		t.Error("the throwaway database should be recreated on start")
	}
	if replaced != "/srv/cinevote/real.db" {
		t.Errorf("replaced = %q, want the configured path so it can be logged", replaced)
	}
	// A password someone chose is still theirs.
	if cfg.AdminPassword != "chosen-by-a-human" {
		t.Errorf("admin password = %q, want the configured one", cfg.AdminPassword)
	}
}
