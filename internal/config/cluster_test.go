package config

import (
	"fmt"
	"testing"
	"time"
)

func TestQuorumSize(t *testing.T) {
	cases := []struct {
		peers int
		want  int
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 2},
		{4, 3},
		{5, 3},
		{7, 4},
	}
	for _, tc := range cases {
		c := &ClusterConfig{}
		for i := 0; i < tc.peers; i++ {
			c.Peers = append(c.Peers, PeerInfo{NodeID: fmt.Sprintf("n%d", i)})
		}
		if got := c.QuorumSize(); got != tc.want {
			t.Errorf("peers=%d: QuorumSize=%d want %d", tc.peers, got, tc.want)
		}
	}
}

func TestClusterMutateBumpsVersion(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	c := &ClusterConfig{}

	err := c.Mutate("nodeA", func(cc *ClusterConfig) error {
		cc.Checks = append(cc.Checks, Check{ID: "1", Name: "x"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 {
		t.Errorf("Version=%d want 1", c.Version)
	}
	if c.UpdatedBy != "nodeA" {
		t.Errorf("UpdatedBy=%q want nodeA", c.UpdatedBy)
	}

	err = c.Mutate("nodeB", func(cc *ClusterConfig) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 2 {
		t.Errorf("Version=%d want 2 after second mutate", c.Version)
	}
}

func TestClusterReplaceGatesOnVersion(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	cur := &ClusterConfig{Version: 5, Checks: []Check{{ID: "old"}}}

	if applied, _ := cur.Replace(&ClusterConfig{Version: 4}); applied {
		t.Error("older version was applied")
	}
	if applied, _ := cur.Replace(&ClusterConfig{Version: 5}); applied {
		t.Error("equal version was applied")
	}
	applied, err := cur.Replace(&ClusterConfig{
		Version: 6,
		Checks:  []Check{{ID: "new"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Error("newer version was not applied")
	}
	if cur.Version != 6 || len(cur.Checks) != 1 || cur.Checks[0].ID != "new" {
		t.Errorf("after replace: %+v", cur)
	}
}

func TestClusterSnapshotIsCopy(t *testing.T) {
	c := &ClusterConfig{Checks: []Check{{ID: "a"}}}
	snap := c.Snapshot()
	snap.Checks[0].ID = "b"
	if c.Checks[0].ID != "a" {
		t.Error("snapshot mutation leaked back to original")
	}
}

func TestFindAlert(t *testing.T) {
	c := &ClusterConfig{Alerts: []Alert{
		{ID: "id-1", Name: "primary", Type: AlertSMTP},
		{ID: "id-2", Name: "secondary", Type: AlertDiscord},
	}}
	if a := c.FindAlert("primary"); a == nil || a.Type != AlertSMTP {
		t.Errorf("by name: %+v", a)
	}
	if a := c.FindAlert("id-2"); a == nil || a.Type != AlertDiscord {
		t.Errorf("by id: %+v", a)
	}
	if a := c.FindAlert("ghost"); a != nil {
		t.Errorf("expected nil for missing, got %+v", a)
	}
}

// TestPendingEnrollmentRoundtrip ensures the new field survives the
// YAML Save/Load cycle including the optional PendingJoin pointer.
func TestPendingEnrollmentRoundtrip(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	c := &ClusterConfig{}
	err := c.Mutate("self", func(cc *ClusterConfig) error {
		cc.PendingEnrollments = []PendingEnrollment{
			{
				ID:          "tok-1",
				Name:        "bravo",
				SecretHash:  "sha256:abc",
				AutoApprove: true,
				CreatedBy:   "self",
				CreatedAt:   time.Now().UTC().Truncate(time.Second),
				ExpiresAt:   time.Now().Add(time.Hour).UTC().Truncate(time.Second),
				PendingJoin: &PendingJoin{
					NodeID:      "joiner",
					Advertise:   "joiner:9901",
					Fingerprint: "sha256:def",
					CertPEM:     "PEM",
					SubmittedAt: time.Now().UTC().Truncate(time.Second),
				},
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadClusterConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(loaded.PendingEnrollments); got != 1 {
		t.Fatalf("after reload: enrollments=%d want 1", got)
	}
	e := loaded.PendingEnrollments[0]
	if e.ID != "tok-1" || e.Name != "bravo" || !e.AutoApprove {
		t.Errorf("header fields lost: %+v", e)
	}
	if e.PendingJoin == nil || e.PendingJoin.NodeID != "joiner" {
		t.Errorf("pending join lost: %+v", e.PendingJoin)
	}
}

// TestFindEnrollmentByIDIsCaseSensitive verifies the lookup is exact
// (operators occasionally typo a name; we don't want a near-match to
// validate the wrong token).
func TestFindEnrollmentByIDIsCaseSensitive(t *testing.T) {
	c := &ClusterConfig{
		PendingEnrollments: []PendingEnrollment{
			{ID: "abcDEF", SecretHash: "sha256:x"},
		},
	}
	if got := c.FindEnrollmentByID("abcDEF"); got == nil {
		t.Error("exact match missed")
	}
	if got := c.FindEnrollmentByID("abcdef"); got != nil {
		t.Error("case-insensitive match accepted")
	}
}
