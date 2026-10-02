package d1store

import (
	"context"
	"encoding/json"
	"testing"
)

// A daemon that has been replaced should retire on its own, so the host running
// it is not left holding a session that serves nothing. The comparison that
// decides this is the start time: without it the fresh daemon reads the record of
// the one it replaced and stands itself down immediately (CHANGE-081).
func TestSuccessorIsOnlyTheNewerClaim(t *testing.T) {
	ctx := context.Background()
	fake := newFakeCloudflare("cf-token")
	c, _ := testClient(t, fake)

	if err := c.ClaimHandover(ctx, "old", "https://old.trycloudflare.com", "sha1", 1000); err != nil {
		t.Fatal(err)
	}
	// We started at 1000, so the record we just wrote is our own.
	if _, replaced, err := c.Successor(ctx, 1000); err != nil || replaced {
		t.Fatalf("our own claim read as a successor: replaced=%v err=%v", replaced, err)
	}
	// We started before it, so it is the successor.
	if err := c.ClaimHandover(ctx, "new", "https://new.trycloudflare.com", "sha2", 2000); err != nil {
		t.Fatal(err)
	}
	claim, replaced, err := c.Successor(ctx, 1000)
	if err != nil || !replaced {
		t.Fatalf("the newer claim was not seen: replaced=%v err=%v", replaced, err)
	}
	if claim.Instance != "new" || claim.Tunnel != "https://new.trycloudflare.com" || claim.Version != "sha2" {
		t.Fatalf("successor = %+v", claim)
	}
	// A record written at exactly our start time is not newer, so a fresh daemon
	// does not retire against a claim it might have written itself.
	if err := c.ClaimHandover(ctx, "tie", "https://tie.trycloudflare.com", "sha3", 3000); err != nil {
		t.Fatal(err)
	}
	if _, replaced, _ := c.Successor(ctx, 3000); replaced {
		t.Fatal("a claim stamped at our own start time counted as a successor")
	}
}

func TestNoClaimMeansNoSuccessor(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	c, _ := testClient(t, fake)
	if _, replaced, err := c.Successor(context.Background(), 1000); err != nil || replaced {
		t.Fatalf("empty state reported a successor: replaced=%v err=%v", replaced, err)
	}
}

// A record that is not the shape we wrote is ignored rather than crashing the
// daemon: the key is shared state and something else may own it.
func TestUnreadableClaimIsNotASuccessor(t *testing.T) {
	ctx := context.Background()
	fake := newFakeCloudflare("cf-token")
	c, _ := testClient(t, fake)
	if _, err := c.query(ctx,
		`INSERT INTO state(key, value, updated_at) VALUES(?, ?, unixepoch())`,
		[]string{HandoverKey, "not json at all"}); err != nil {
		t.Fatal(err)
	}
	if _, replaced, err := c.Successor(ctx, 1000); err != nil || replaced {
		t.Fatalf("garbage was read as a successor: replaced=%v err=%v", replaced, err)
	}
}

func TestClaimOverwritesTheSingleKey(t *testing.T) {
	ctx := context.Background()
	fake := newFakeCloudflare("cf-token")
	c, _ := testClient(t, fake)
	if err := c.ClaimHandover(ctx, "a", "https://a.example", "", 10); err != nil {
		t.Fatal(err)
	}
	if err := c.ClaimHandover(ctx, "b", "https://b.example", "", 20); err != nil {
		t.Fatal(err)
	}
	res, err := c.query(ctx, "SELECT value FROM state WHERE key = ?", []string{HandoverKey})
	if err != nil {
		t.Fatal(err)
	}
	list, err := queryInto[struct{ Value string }](res.rows)
	if err != nil || len(list) == 0 {
		t.Fatalf("read back: %v", err)
	}
	stored := list[0].Value
	var got Handover
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatal(err)
	}
	if got.Instance != "b" {
		t.Fatalf("the key still holds %q", got.Instance)
	}
}
