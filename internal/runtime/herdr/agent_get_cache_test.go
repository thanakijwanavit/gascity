package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unnamedRegistryFake writes a fake herdr shaped like herdr 0.9.3 on G1
// (g1-brru): `agent get <name>` answers agent_not_found, and `agent list`
// lists one pane keyed only by its kind. Each call's verb pair is appended to
// the returned log.
func unnamedRegistryFake(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "herdr")
	script := "#!/bin/sh\nshift 2\necho \"$1 $2\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"  'agent get') echo '{\"error\":{\"code\":\"agent_not_found\",\"message\":\"agent target '\"$3\"' not found\"}}' ;;\n" +
		"  'agent list') echo '{\"result\":{\"agents\":[{\"agent\":\"claude\",\"pane_id\":\"w1:p1\",\"agent_status\":\"idle\"}]}}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func verbCounts(t *testing.T, log string) map[string]int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			counts[line]++
		}
	}
	return counts
}

// A name herdr cannot resolve is asked about once, not on every lookup: the
// repeat lookups answer from one shared `agent list` (g1-brru: these failing
// `agent get` calls were ~98% of all herdr requests on G1).
func TestGetAgentRemembersNotFoundAndSharesTheList(t *testing.T) {
	bin, log := unnamedRegistryFake(t)
	c := &client{session: "gc-test", bin: bin}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, ok, err := c.getAgent(ctx, "mayor"); err != nil || ok {
			t.Fatalf("getAgent(mayor) #%d = ok=%v, %v; want absent", i, ok, err)
		}
	}
	got := verbCounts(t, log)
	if got["agent get"] != 1 || got["agent list"] != 1 {
		t.Fatalf("herdr calls = %v; want one agent get and one agent list", got)
	}
}

// A registry change made through the client forgets the misses, so an agent
// it just started is asked about again at once.
func TestGetAgentAsksAgainAfterARegistryChange(t *testing.T) {
	bin, log := unnamedRegistryFake(t)
	c := &client{session: "gc-test", bin: bin}
	ctx := context.Background()
	if _, _, err := c.getAgent(ctx, "mayor"); err != nil {
		t.Fatal(err)
	}
	_, _ = c.run(ctx, "agent", "start", "mayor", "--kind", "claude", "--pane", "w1:p1")
	if _, _, err := c.getAgent(ctx, "mayor"); err != nil {
		t.Fatal(err)
	}
	got := verbCounts(t, log)
	if got["agent get"] != 2 || got["agent list"] != 2 {
		t.Fatalf("herdr calls = %v; want agent get and agent list asked again after agent start", got)
	}
}

// Misses and the list snapshot expire, so herdr versions that do resolve names
// are re-checked.
func TestGetAgentMissExpires(t *testing.T) {
	bin, log := unnamedRegistryFake(t)
	c := &client{session: "gc-test", bin: bin}
	ctx := context.Background()
	if _, _, err := c.getAgent(ctx, "mayor"); err != nil {
		t.Fatal(err)
	}
	c.agentsMu.Lock()
	c.getMiss["mayor"] = time.Now().Add(-agentGetMissTTL)
	c.agentsAt = time.Now().Add(-agentListCacheTTL)
	c.agentsMu.Unlock()
	if _, _, err := c.getAgent(ctx, "mayor"); err != nil {
		t.Fatal(err)
	}
	got := verbCounts(t, log)
	if got["agent get"] != 2 || got["agent list"] != 2 {
		t.Fatalf("herdr calls = %v; want both asked again once the miss expired", got)
	}
}
