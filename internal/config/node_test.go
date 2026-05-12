package config

import "testing"

func TestAdvertiseAddrFallback(t *testing.T) {
	cases := []struct {
		name      string
		cfg       NodeConfig
		want      string
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
