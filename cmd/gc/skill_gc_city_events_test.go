package main

import (
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The core gc-city skill is what a seat reads before scripting `gc events`.
// Keep its examples runnable (every flag exists on the real command) and keep
// the output contract it states (JSON Lines vs the plain-text --seq cursor,
// async completion via event_cursor + request_id) from silently going away.
func TestGCCitySkillEventsExamplesUseRealFlags(t *testing.T) {
	data, err := os.ReadFile("../../internal/bootstrap/packs/core/skills/gc-city/SKILL.md")
	if err != nil {
		t.Fatalf("read gc-city skill: %v", err)
	}
	skill := string(data)

	cmd := newEventsCmd(io.Discard, io.Discard)
	globals := map[string]bool{"city": true, "rig": true}
	flagRE := regexp.MustCompile(`--([a-z][a-z-]*)`)
	examples := 0
	for _, line := range strings.Split(skill, "\n") {
		code, _, _ := strings.Cut(line, "#")
		code = strings.TrimSpace(code)
		if !strings.HasPrefix(code, "gc events") {
			continue
		}
		examples++
		for _, m := range flagRE.FindAllStringSubmatch(code, -1) {
			if cmd.Flags().Lookup(m[1]) == nil && !globals[m[1]] {
				t.Errorf("gc-city skill example %q uses --%s, which gc events does not define", code, m[1])
			}
		}
	}
	if examples < 5 {
		t.Fatalf("gc-city skill has %d gc events examples; want list, filter, follow, watch and --seq", examples)
	}

	for _, want := range []string{
		"JSON Lines",           // list/watch/follow framing
		"`--seq` is the one",   // --seq is the plain-text exception
		"--after-cursor",       // supervisor cursor resume
		"event_cursor",         // async 202 response field
		"request_id",           // async completion correlation
		"docs/reference/events.md",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("gc-city skill no longer documents %q", want)
		}
	}
}
