package config

import "testing"

func TestAdvertiseAddrFallback(t *testing.T) {
	cases := []struct {
		name string
		cfg  NodeConfig
		want string
	}{
		{"explicit advertise wins", NodeConfig{Advertise: "host:1234", BindAddr: "0.0.0.0", BindPort: 9901}, "host:1234"},
		{"empty bind falls back to loopback", NodeConfig{BindPort: 9901}, "127.0.0.1:9901"},
		{"wildcard bind falls back to loopback", NodeConfig{BindAddr: "0.0.0.0", BindPort: 9901}, "127.0.0.1:9901"},
		{"ipv6 wildcard falls back to loopback", NodeConfig{BindAddr: "::", BindPort: 9901}, "127.0.0.1:9901"},
		{"specific bind preserved", NodeConfig{BindAddr: "10.0.0.1", BindPort: 9901}, "10.0.0.1:9901"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.AdvertiseAddr(); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNodeConfigRoundtrip(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	n := &NodeConfig{NodeID: "abc", BindAddr: "127.0.0.1", BindPort: 9901, Advertise: "10.0.0.1:9901"}
	if err := n.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadNodeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if *loaded != *n {
		t.Errorf("got %+v want %+v", *loaded, *n)
	}
}

func TestLoadNodeConfigAppliesDefaults(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	// Save with empty bind addr/port to verify Load fills them.
	n := &NodeConfig{NodeID: "abc"}
	if err := n.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadNodeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BindPort != 9901 {
		t.Errorf("BindPort=%d want 9901", loaded.BindPort)
	}
	if loaded.BindAddr != "0.0.0.0" {
		t.Errorf("BindAddr=%q want 0.0.0.0", loaded.BindAddr)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv(EnvNodeID, "node-from-env")
	t.Setenv(EnvBindAddr, "1.2.3.4")
	t.Setenv(EnvBindPort, "9999")
	t.Setenv(EnvAdvertise, "public.example.com:9999")
	// QUPTIME_CLUSTER_SECRET is intentionally ignored post-enrollment
	// rework; setting it must NOT touch the field below.
	t.Setenv(EnvClusterSecret, "shh-secret")

	n := &NodeConfig{
		NodeID:        "original-id",
		BindAddr:      "0.0.0.0",
		BindPort:      9901,
		Advertise:     "old.example.com:9901",
		ClusterSecret: "old-secret",
	}
	if err := n.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	want := NodeConfig{
		NodeID:        "node-from-env",
		BindAddr:      "1.2.3.4",
		BindPort:      9999,
		Advertise:     "public.example.com:9999",
		ClusterSecret: "old-secret", // env override no longer applies
	}
	if *n != want {
		t.Errorf("got %+v want %+v", *n, want)
	}
}

func TestApplyEnvOverridesEmptyValuesIgnored(t *testing.T) {
	// Explicitly empty env vars must NOT clobber existing fields —
	// otherwise `docker run -e QUPTIME_ADVERTISE=` would silently
	// erase a previously-persisted advertise address.
	t.Setenv(EnvNodeID, "")
	t.Setenv(EnvBindAddr, "")
	t.Setenv(EnvBindPort, "")
	t.Setenv(EnvAdvertise, "")
	t.Setenv(EnvClusterSecret, "")

	orig := NodeConfig{
		NodeID:        "keep-me",
		BindAddr:      "10.0.0.1",
		BindPort:      9901,
		Advertise:     "keep.example.com:9901",
		ClusterSecret: "keep-secret",
	}
	n := orig
	if err := n.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if n != orig {
		t.Errorf("empty env vars mutated config: got %+v want %+v", n, orig)
	}
}

func TestApplyEnvOverridesBadPort(t *testing.T) {
	t.Setenv(EnvBindPort, "not-an-int")
	n := &NodeConfig{}
	if err := n.ApplyEnvOverrides(); err == nil {
		t.Fatal("expected error for non-integer port")
	}
}

func TestLoadNodeConfigEnvOverridesFile(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	// Persist a file with one bind addr; env should win on load.
	n := &NodeConfig{NodeID: "abc", BindAddr: "127.0.0.1", BindPort: 9901, Advertise: "file.example.com:9901"}
	if err := n.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBindAddr, "0.0.0.0")
	t.Setenv(EnvAdvertise, "env.example.com:9001")
	t.Setenv(EnvBindPort, "9001")

	loaded, err := LoadNodeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BindAddr != "0.0.0.0" {
		t.Errorf("BindAddr=%q want 0.0.0.0 (env override)", loaded.BindAddr)
	}
	if loaded.BindPort != 9001 {
		t.Errorf("BindPort=%d want 9001 (env override)", loaded.BindPort)
	}
	if loaded.Advertise != "env.example.com:9001" {
		t.Errorf("Advertise=%q want env.example.com:9001 (env override)", loaded.Advertise)
	}
	if loaded.NodeID != "abc" {
		t.Errorf("NodeID=%q want abc (unchanged)", loaded.NodeID)
	}
}
