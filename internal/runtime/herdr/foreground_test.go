package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── g1-j3yf1: nothing is typed into a shell, and a shell pane is no live session ──
//
// On 29 Oct 2026 G1 rebooted. herdr restored the agent panes and typed
// `claude --resume <id>` into each; the resume found no conversation and
// exited, leaving bash at its prompt. gc still called those sessions active (a
// raw-launched session read as running for as long as its pane existed) and
// pasted its nudges, mail and room-line previews that can carry a customer's
// words, into bash and pressed Enter, so every line ran as a shell command.
//
// These tests pin both halves of the fix against the fake herdr. Text goes only
// to a pane whose foreground process group holds the session's agent: a shell,
// anything else, or a foreground that cannot be read gets nothing typed and an
// error. And a raw session whose pane is back at a bare interactive prompt reads
// not running, so the controller restarts it. The process shapes are the ones
// herdr 0.9.3 reported on G1 for a live claude pane and for the failed-resume
// pane.

const guardSession = "mayor"

// guardNudge reads like a room-line preview carrying shell syntax: a bash prompt
// would run it if it were ever typed there.
const guardNudge = `Room: "ok" > /tmp/g1-j3yf1-pwned; $(touch /tmp/g1-j3yf1-pwned)`

func fgProc(pid int, name string, argv ...string) proc {
	return proc{PID: pid, Name: name, Argv: argv}
}

// What herdr 0.9.3 reported on G1: the failed-resume pane (bash at its prompt,
// nothing else) and a live raw-launched claude (the `exec /bin/sh -c` wrapper as
// the pane's root, claude and its MCP helpers below it).
var (
	bareBashPID   = 304098
	bareBash      = []proc{fgProc(304098, "bash", "bash")}
	liveClaudePID = 980276
	liveClaude    = []proc{
		fgProc(980276, "sh", "/bin/sh", "-c", "CLAUDE_CONFIG_DIR=/cfg claude --resume k1"),
		fgProc(980346, "claude", "claude", "--resume", "k1"),
		fgProc(982912, "npm exec @playw", "mcp@latest"),
		fgProc(984392, "MainThread", "node", "/srv/mcp.js"),
	}
)

// setForeground makes the fake herdr answer every `pane process-info` with
// shellPID and fg until the test sets another.
func setForeground(t *testing.T, state string, shellPID int, fg ...proc) {
	t.Helper()
	var reply struct {
		Result struct {
			ProcessInfo struct {
				ShellPID   int    `json:"shell_pid"`
				Foreground []proc `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	reply.Result.ProcessInfo.ShellPID = shellPID
	reply.Result.ProcessInfo.Foreground = append([]proc{}, fg...)
	b, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	setForegroundReply(t, state, string(b))
}

// setForegroundReply makes the fake herdr print reply, as it is, for every
// `pane process-info`.
func setForegroundReply(t *testing.T, state, reply string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, "foreground.json"), []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
}

// legacyRawBinding seeds the sidecar the way the binary installed on G1 left it
// for a raw-launched claude session: shell mode, the pane, the exact name, and
// the GC_PROVIDER the session env carried, with none of the keys a later
// binary adds.
func legacyRawBinding(t *testing.T, p *Provider, name string) {
	t.Helper()
	bindTestPane(t, p, name, bindModeShell)
	for k, v := range map[string]string{metaBoundName: name, "GC_PROVIDER": "claude"} {
		if err := p.SetMeta(name, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// rawClaudeCommand is a claude launch the way G1 writes it: the env assignment
// rules out a kind launch, so Start execs it through the pane's shell.
const rawClaudeCommand = "CLAUDE_CONFIG_DIR=/cfg claude --resume k1"

// startSession starts name with cfg against the fake herdr.
func startSession(t *testing.T, p *Provider, name string, cfg runtime.Config) {
	t.Helper()
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// assertNothingTyped fails if any verb that types into a pane appears in the
// fake herdr's calls after offset from.
func assertNothingTyped(t *testing.T, state string, from int) {
	t.Helper()
	calls := fakeCalls(t, state)[from:]
	for _, verb := range []string{"agent prompt", "pane run", "pane send-keys"} {
		if strings.Contains(calls, verb) {
			t.Fatalf("%q was issued, but nothing may be typed into this pane:\n%s", verb, calls)
		}
	}
}

// assertRefused fails unless err refuses the delivery, names the session and
// carries none of the nudge text; wants are further phrases it must contain.
func assertRefused(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the delivery returned nil, so a refused delivery would read as a success")
	}
	msg := err.Error()
	if !strings.Contains(msg, guardSession) {
		t.Errorf("the refusal does not name the session %q: %v", guardSession, err)
	}
	if strings.Contains(msg, "pwned") || strings.Contains(msg, "Room:") {
		t.Errorf("the refusal carries the nudge text, which must never reach a log: %v", err)
	}
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("the refusal does not say %q: %v", w, err)
		}
	}
}

// The incident itself: a nudge to a session whose pane is back at a shell
// prompt is refused with nothing typed, whether the binding was written by the
// binary G1 runs today or by this one, and whichever shell the pane idles in.
func TestNudgeIsNeverTypedIntoAShell(t *testing.T) {
	shells := []struct {
		name     string
		shellPID int
		fg       []proc
	}{
		{"bash after a failed claude --resume (G1 wF5:p1)", bareBashPID, bareBash},
		{"login zsh", 100, []proc{fgProc(100, "-zsh", "-zsh")}},
		{"sh", 100, []proc{fgProc(100, "sh", "/bin/sh")}},
		{"fish login shell", 100, []proc{fgProc(100, "fish", "/usr/bin/fish", "-l")}},
	}
	sessions := []struct {
		name  string
		setup func(t *testing.T, p *Provider)
	}{
		{"binding left by the installed binary", func(t *testing.T, p *Provider) { legacyRawBinding(t, p, guardSession) }},
		{"raw session started by this binary", func(t *testing.T, p *Provider) {
			startSession(t, p, guardSession, runtime.Config{Command: rawClaudeCommand, ProcessNames: []string{"node", "claude"}})
		}},
		{"kind-launched agent that exited", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{Command: "claude"}) }},
	}
	for _, s := range sessions {
		for _, sh := range shells {
			t.Run(s.name+"/"+sh.name, func(t *testing.T) {
				p, state := newFakeHerdrProvider(t)
				listenHerdrSocket(t, p)
				s.setup(t, p)
				setForeground(t, state, sh.shellPID, sh.fg...)
				from := len(fakeCalls(t, state))
				err := p.Nudge(guardSession, runtime.TextContent(guardNudge))
				assertRefused(t, err, "pane runs a shell")
				assertNothingTyped(t, state, from)
			})
		}
	}
}

// Fail closed: a foreground that cannot be read or shows no process is not a
// confirmed agent, so nothing is typed. The session is kind-launched, so the
// registry resolves its pane and no binding probe refuses first.
func TestNudgeRefusesAForegroundItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"process-info fails", `{"error":{"code":"internal_error","message":"process table unavailable"}}`},
		{"no foreground process reported", `{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}`},
		{"no shell pid and no foreground", `{"result":{"process_info":{"shell_pid":0,"foreground_processes":[]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			startSession(t, p, guardSession, runtime.Config{Command: "claude"})
			setForegroundReply(t, state, tc.reply)
			from := len(fakeCalls(t, state))
			assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)))
			assertNothingTyped(t, state, from)
		})
	}
}

// Only the agent's own process counts, matched on its exact name: a shell that
// mentions the agent, a shell that renames itself, a name or path that merely
// contains the agent's, and any other program all get nothing typed.
func TestNudgeRefusesAForegroundThatIsNotTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shellPID int
		fg       []proc
	}{
		{"bash -c claude names claude only in its arguments", 100, []proc{fgProc(100, "bash", "bash", "-c", "claude")}},
		{"a shell whose argv[0] claims to be claude", 100, []proc{fgProc(100, "bash", "claude")}},
		{"a name that only contains claude", 100, []proc{fgProc(100, "sh", "/bin/sh", "-c", "claudex"), fgProc(101, "claudex", "claudex")}},
		{"a path that only contains claude", 100, []proc{fgProc(101, "my-claude", "/opt/bin/my-claude")}},
		{"an editor", 100, []proc{fgProc(101, "vim", "vim", "notes.md")}},
		{"a python REPL", 100, []proc{fgProc(101, "python3", "python3")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			startSession(t, p, guardSession, runtime.Config{Command: "claude"})
			setForeground(t, state, tc.shellPID, tc.fg...)
			from := len(fakeCalls(t, state))
			assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)))
			assertNothingTyped(t, state, from)
		})
	}
}

// The guard must not cost a live agent its nudges: a G1 raw claude takes the
// paste fallback, a claude whose process name is its version string is still
// claude by its argv[0], and a registered agent takes `agent prompt`.
func TestNudgeReachesTheAgentInTheForeground(t *testing.T) {
	t.Run("raw claude on G1, delivered by paste", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		legacyRawBinding(t, p, guardSession)
		setForeground(t, state, liveClaudePID, liveClaude...)
		if err := p.Nudge(guardSession, runtime.TextContent(guardNudge)); err != nil {
			t.Fatalf("Nudge to a live claude: %v", err)
		}
		calls := fakeCalls(t, state)
		if !strings.Contains(calls, "pane run %5 "+guardNudge+"\n") || !strings.Contains(calls, "pane send-keys %5 Enter") {
			t.Fatalf("the nudge did not reach the live claude by paste and Enter:\n%s", calls)
		}
	})
	t.Run("claude whose process name is its version", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		legacyRawBinding(t, p, guardSession)
		setForeground(t, state, liveClaudePID, liveClaude[0], fgProc(980346, "2.1.216", "claude", "--resume", "k1"))
		if err := p.Nudge(guardSession, runtime.TextContent("You have mail.")); err != nil {
			t.Fatalf("Nudge to a claude named by its version: %v", err)
		}
		if calls := fakeCalls(t, state); !strings.Contains(calls, "pane run %5 You have mail.\n") {
			t.Fatalf("the nudge did not reach the claude named by its version:\n%s", calls)
		}
	})
	t.Run("registered agent, delivered by agent prompt", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		listenHerdrSocket(t, p)
		startSession(t, p, guardSession, runtime.Config{Command: "claude"})
		if err := p.Nudge(guardSession, runtime.TextContent("You have mail.")); err != nil {
			t.Fatalf("Nudge to a registered claude: %v", err)
		}
		if calls := fakeCalls(t, state); !strings.Contains(calls, "agent prompt %5 You have mail.\n") {
			t.Fatalf("the nudge did not reach the registered claude:\n%s", calls)
		}
	})
}

// "The session's provider, as configured": a codex session's pane running
// claude is not running its agent, and a shell never is, even where a
// provider's own process names list one (zcode-repl's profile lists bash).
func TestNudgeAcceptsOnlyTheSessionsConfiguredAgent(t *testing.T) {
	t.Run("codex session, claude in the foreground", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		listenHerdrSocket(t, p)
		startSession(t, p, guardSession, runtime.Config{Command: "codex", ProcessNames: []string{"codex"}})
		setForeground(t, state, 4242, fgProc(4243, "claude", "claude"))
		from := len(fakeCalls(t, state))
		assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)))
		assertNothingTyped(t, state, from)

		setForeground(t, state, 4242, fgProc(4243, "codex", "codex"))
		if err := p.Nudge(guardSession, runtime.TextContent("You have mail.")); err != nil {
			t.Fatalf("Nudge to the codex it was configured for: %v", err)
		}
		if calls := fakeCalls(t, state); !strings.Contains(calls, "agent prompt %5 You have mail.\n") {
			t.Fatalf("the nudge did not reach codex:\n%s", calls)
		}
	})
	t.Run("a shell listed among the process names", func(t *testing.T) {
		p, state := newFakeHerdrProvider(t)
		listenHerdrSocket(t, p)
		startSession(t, p, guardSession, runtime.Config{Command: "zcode-repl", ProcessNames: []string{"bash", "node"}})
		setForeground(t, state, bareBashPID, bareBash...)
		from := len(fakeCalls(t, state))
		assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)), "pane runs a shell")
		assertNothingTyped(t, state, from)
	})
}

// A session whose shell is the session (its life launched no command) takes its
// nudges at that shell's prompt, as the runtime contract's nudge to a running
// session expects; anything else in its foreground still gets nothing. A G1
// binding, whose env named a provider, never counts as one (the incident test
// above).
func TestNudgeTypesIntoTheShellOfAShellSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *Provider)
		fg    []proc
	}{
		{"empty command", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{}) }, bareBash},
		{"command that is an interactive shell", func(t *testing.T, p *Provider) {
			startSession(t, p, guardSession, runtime.Config{Command: "bash -l"})
		}, []proc{fgProc(bareBashPID, "bash", "bash", "-l")}},
		{"binding with no provider, left by the installed binary", func(t *testing.T, p *Provider) {
			bindTestPane(t, p, guardSession, bindModeShell)
		}, bareBash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			tc.setup(t, p)
			setForeground(t, state, bareBashPID, tc.fg...)
			if err := p.Nudge(guardSession, runtime.TextContent("echo hello")); err != nil {
				t.Fatalf("Nudge to a shell session at its prompt: %v", err)
			}
			if calls := fakeCalls(t, state); !strings.Contains(calls, "pane run %5 echo hello\n") {
				t.Fatalf("the nudge did not reach the shell session's prompt:\n%s", calls)
			}

			setForeground(t, state, bareBashPID, fgProc(bareBashPID+1, "vim", "vim", "notes.md"))
			from := len(fakeCalls(t, state))
			assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)), "pane runs vim")
			assertNothingTyped(t, state, from)
		})
	}
}

// A launch record that cannot be read proves nothing either way, and each
// reader takes its safe side: the type check stays strict (a bare prompt takes
// no text even in a session recorded as a shell session), while liveness keeps
// the session running rather than restart it on a guess.
func TestUnreadableLaunchRecordTakesTheSafeSideForEachReader(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file; the unreadable sidecar cannot be modelled")
	}
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	startSession(t, p, guardSession, runtime.Config{})
	setForeground(t, state, bareBashPID, bareBash...)
	record := filepath.Join(p.metaDir, sanitize(guardSession), sanitize(metaLaunchedCommand))
	if err := os.Chmod(record, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(record, 0o600) })

	from := len(fakeCalls(t, state))
	assertRefused(t, p.Nudge(guardSession, runtime.TextContent(guardNudge)), "pane runs a shell")
	assertNothingTyped(t, state, from)
	if !p.IsRunning(guardSession) {
		t.Fatal("IsRunning = false on an unreadable launch record; a liveness read must not restart a session on a guess")
	}
}

// Liveness: a raw session whose pane is back at a bare interactive prompt is not
// running, so the reconciler restarts it the way it restarts an exited agent.
// Nothing reaps the pane here: it still resolves, so Peek reads it and Stop
// closes it, and Start recycles its tab.
func TestRawSessionAtAShellPromptIsNotRunning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *Provider)
	}{
		{"binding left by the installed binary", func(t *testing.T, p *Provider) { legacyRawBinding(t, p, guardSession) }},
		{"raw session started by this binary", func(t *testing.T, p *Provider) {
			startSession(t, p, guardSession, runtime.Config{Command: rawClaudeCommand, ProcessNames: []string{"node", "claude"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			tc.setup(t, p)

			setForeground(t, state, liveClaudePID, liveClaude...)
			if got := p.ObserveLiveness(guardSession, nil); !got.Running || !got.Alive {
				t.Fatalf("ObserveLiveness = %+v for a live claude; want Running and Alive", got)
			}

			setForeground(t, state, bareBashPID, bareBash...)
			if got := p.ObserveLiveness(guardSession, []string{"node", "claude"}); got.Running || got.Alive {
				t.Fatalf("ObserveLiveness = %+v for a pane at a bash prompt; want not running, so the controller restarts it", got)
			}
			if p.IsRunning(guardSession) {
				t.Fatal("IsRunning = true for a pane at a bash prompt")
			}
			names, err := p.ListRunning("")
			if err != nil {
				t.Fatalf("ListRunning: %v", err)
			}
			for _, n := range names {
				if n == guardSession {
					t.Fatalf("ListRunning = %v lists a session whose pane is at a bash prompt", names)
				}
			}
			if calls := fakeCalls(t, state); strings.Contains(calls, "pane close") || strings.Contains(calls, "tab close") {
				t.Fatalf("a liveness read closed the pane; only Stop and Start's recycle may:\n%s", calls)
			}
			if _, err := p.Peek(guardSession, 10); err != nil {
				t.Fatalf("Peek no longer reaches the pane: %v", err)
			}
			if err := p.Stop(guardSession); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if calls := fakeCalls(t, state); !strings.Contains(calls, "pane close %5") {
				t.Fatalf("Stop did not close the pane:\n%s", calls)
			}
		})
	}
}

// What must keep running at a prompt: a session whose shell is the session
// (empty command, or a command that is itself an interactive shell), a binding
// with nothing saying an agent was launched, and a live raw session whose
// foreground momentarily shows only its `sh -c` wrapper.
func TestSessionsWhoseShellIsTheSessionStayRunning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, p *Provider)
		shellPID int
		fg       []proc
	}{
		{"empty command", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{}) }, bareBashPID, bareBash},
		{"command that is an interactive shell", func(t *testing.T, p *Provider) { startSession(t, p, guardSession, runtime.Config{Command: "bash -l"}) }, 4242, []proc{fgProc(4242, "bash", "bash", "-l")}},
		{"binding with no provider, left by the installed binary", func(t *testing.T, p *Provider) {
			bindTestPane(t, p, guardSession, bindModeShell)
			if err := p.SetMeta(guardSession, metaBoundName, guardSession); err != nil {
				t.Fatal(err)
			}
		}, bareBashPID, bareBash},
		{"raw session between two commands", func(t *testing.T, p *Provider) { legacyRawBinding(t, p, guardSession) }, liveClaudePID, liveClaude[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			tc.setup(t, p)
			setForeground(t, state, tc.shellPID, tc.fg...)
			if !p.IsRunning(guardSession) {
				t.Fatal("IsRunning = false; this session runs at its prompt, and reading it dead restarts it every tick")
			}
			if got := p.ObserveLiveness(guardSession, nil); !got.Running || !got.Alive {
				t.Fatalf("ObserveLiveness = %+v; want Running and Alive", got)
			}
		})
	}
}

// The startup turn takes the same guard: a kind-launched claude that exited at
// once (a resume that found no conversation) leaves bash in the pane, and the
// first turn must not be typed there. Start still succeeds (the reconciler
// owns the restart) and the strand is recorded with the reason.
func TestStartupTurnIsNeverTypedIntoAShell(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setForeground(t, state, bareBashPID, bareBash...)

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), guardSession, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "agent prompt %5 Run gc hook") || strings.Contains(calls, "pane run %5 Run gc hook") {
		t.Fatalf("the startup turn was typed into a pane at a bash prompt:\n%s", calls)
	}
	v, err := p.GetMeta(guardSession, metaStartupUnconfirmed)
	if err != nil || !strings.Contains(v, "pane runs a shell") {
		t.Fatalf("startup marker = %q, %v; want the strand recorded as a shell pane", v, err)
	}
}

// allowTyping is a type check every pane passes, for client tests about what
// follows the check.
func allowTyping(context.Context, string) error { return nil }

// ── the pieces, case by case ─────────────────────────────────────────────────

// A bare prompt is the root shell alone in the foreground and started as an
// interactive shell; a raw launch's `sh -c` wrapper, a shell running a script,
// a foreground without argv, or anything beside the root is not.
func TestBarePromptShell(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shellPID int
		fg       []proc
		shell    string
	}{
		{"G1 failed-resume pane: bash, argv [bash]", bareBashPID, bareBash, "bash"},
		{"login shell", 100, []proc{fgProc(100, "-zsh", "-zsh")}, "zsh"},
		{"shell with interactive flags", 100, []proc{fgProc(100, "bash", "/bin/bash", "--login", "-i")}, "bash"},
		{"G1 live claude: wrapper and agent", liveClaudePID, liveClaude, ""},
		{"raw wrapper alone between two commands", liveClaudePID, liveClaude[:1], ""},
		{"shell running a script", 100, []proc{fgProc(100, "bash", "bash", "/srv/run.sh")}, ""},
		{"shell given -lc", 100, []proc{fgProc(100, "bash", "bash", "-lc", "claude")}, ""},
		{"argv not reported", 100, []proc{fgProc(100, "bash")}, ""},
		{"root is not a shell", 100, []proc{fgProc(100, "claude", "claude")}, ""},
		{"argv[0] is not a shell", 100, []proc{fgProc(100, "bash", "claude")}, ""},
		{"a job beside the shell", 100, []proc{fgProc(100, "bash", "bash"), fgProc(101, "sleep", "sleep", "5")}, ""},
		{"a shell that is not the root", 100, []proc{fgProc(101, "bash", "bash")}, ""},
		{"no foreground", 100, nil, ""},
		{"no shell pid", 0, []proc{fgProc(0, "bash", "bash")}, ""},
	} {
		shell, bare := barePromptShell(tc.shellPID, tc.fg)
		if shell != tc.shell || bare != (tc.shell != "") {
			t.Errorf("%s: barePromptShell = %q, %v; want %q, %v", tc.name, shell, bare, tc.shell, tc.shell != "")
		}
		if got := paneProbeFrom(tc.shellPID, tc.fg).Bare; got != bare {
			t.Errorf("%s: paneProbeFrom(...).Bare = %v; want %v", tc.name, got, bare)
		}
	}
}

// The agent is matched exactly, on a process's name or its argv[0]'s program
// name, and never as a shell.
func TestAgentInForeground(t *testing.T) {
	claude := []string{"node", "claude"}
	for _, tc := range []struct {
		name string
		fg   []proc
		want []string
		ok   bool
	}{
		{"G1 live claude", liveClaude, claude, true},
		{"claude named by its version", []proc{fgProc(7, "2.1.216", "claude")}, claude, true},
		{"claude by the path in argv[0]", []proc{fgProc(7, "2.1.216", "/home/u/.local/bin/claude")}, claude, true},
		{"a configured node agent", []proc{fgProc(7, "node", "node", "cli.js")}, claude, true},
		{"bare bash", bareBash, claude, false},
		{"bash -c claude", []proc{fgProc(7, "bash", "bash", "-c", "claude")}, claude, false},
		{"bash claiming to be claude", []proc{fgProc(7, "bash", "claude")}, claude, false},
		{"login shell claiming to be claude", []proc{fgProc(7, "-zsh", "claude")}, claude, false},
		{"claudex", []proc{fgProc(7, "claudex", "claudex")}, claude, false},
		{"pid 0", []proc{fgProc(0, "claude", "claude")}, claude, false},
		{"no foreground", nil, claude, false},
		{"nothing accepted", liveClaude, nil, false},
	} {
		if got := agentInForeground(tc.fg, tc.want); got != tc.ok {
			t.Errorf("%s: agentInForeground = %v; want %v", tc.name, got, tc.ok)
		}
	}
}

// A session's shell is the session for an empty command and for a command that
// is an interactive shell; every other launch runs a command.
func TestLaunchesCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"":                  false,
		"bash":              false,
		"bash -l":           false,
		"/bin/zsh --login":  false,
		"claude":            true,
		"claude --resume":   true,
		rawClaudeCommand:    true,
		"python3 worker.py": true,
		"bash -c 'claude'":  true,
		"bash run.sh":       true,
		"zcode-repl":        true,
	} {
		if got := launchesCommand(launchSpecFor(cmd)); got != want {
			t.Errorf("launchesCommand(%q) = %v; want %v", cmd, got, want)
		}
	}
}

// The configured names less shells, or the agent kinds when none remain.
func TestAgentProcessNames(t *testing.T) {
	if got := agentProcessNames([]string{"node", " claude ", ""}); !slices.Equal(got, []string{"node", "claude"}) {
		t.Errorf("configured names = %v; want [node claude]", got)
	}
	if got := agentProcessNames([]string{"bash", "node"}); !slices.Equal(got, []string{"node"}) {
		t.Errorf("a shell among the names = %v; want [node]", got)
	}
	kinds := agentProcessNames([]string{"bash", "-zsh"})
	for _, k := range []string{"claude", "codex", "kimi"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("fallback %v lacks the agent kind %q", kinds, k)
		}
	}
	for _, sh := range []string{"bash", "zsh", "sh"} {
		if slices.Contains(kinds, sh) {
			t.Errorf("fallback %v accepts the shell %q", kinds, sh)
		}
	}
	if got := agentProcessNames(nil); !slices.Equal(got, kinds) {
		t.Errorf("no names = %v; want the agent kinds %v", got, kinds)
	}
}

// The shell-mode rule on its own: not running only at a bare prompt of a life
// that launched a command, and nothing reaped or cleared either way.
func TestResolveBindingShellModeAtBarePrompt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		probe    paneProbe
		launched bool
		running  bool
	}{
		{"launched command, bare prompt", paneProbe{Exists: true, Bare: true}, true, false},
		{"shell is the session, bare prompt", paneProbe{Exists: true, Bare: true}, false, true},
		{"launched command, neither busy nor bare", paneProbe{Exists: true}, true, true},
		{"launched command, busy", paneProbe{Exists: true, Busy: true}, true, true},
	} {
		rec := &resolveOpsRec{}
		ops := opsForRec(t, false, nil, "%5", bindModeShell, tc.probe, nil, rec)
		ops.launchedCommand = func() bool { return tc.launched }
		pane, running, err := resolveBinding(ops)
		if err != nil || pane != "%5" || running != tc.running {
			t.Errorf("%s: resolveBinding = %q, %v, %v; want %%5, %v, nil", tc.name, pane, running, err, tc.running)
		}
		if rec.reaped != "" || rec.cleared {
			t.Errorf("%s: a shell-mode pane was reaped (%q) or its binding cleared (%v)", tc.name, rec.reaped, rec.cleared)
		}
	}
}

// Start records, before the launch, whether the life runs a command and which
// processes are its agent, and a prior life's names do not outlive a life that
// configured none.
func TestStartRecordsWhatTheLaunchRuns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       runtime.Config
		prior     string
		launched  string
		processes string
	}{
		{"raw claude", runtime.Config{Command: rawClaudeCommand, ProcessNames: []string{"node", "claude"}}, "", "true", "node\nclaude"},
		{"empty command", runtime.Config{}, "", "false", ""},
		{"kind launch after a life that named codex", runtime.Config{Command: "claude"}, "codex", "true", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			if tc.prior != "" {
				if err := p.SetMeta(guardSession, metaAgentProcesses, tc.prior); err != nil {
					t.Fatal(err)
				}
			}
			rewriteFake(t, p, `if [ -e "$METADIR/$3/GC_HERDR_PANE_ID" ]; then : > "$STATE/bound_before_launch"; fi`,
				`if [ -e "$METADIR/$3/GC_HERDR_PANE_ID" ]; then : > "$STATE/bound_before_launch"; fi
    if [ -e "$METADIR/$3/GC_HERDR_LAUNCHED_COMMAND" ]; then : > "$STATE/launch_recorded_before_launch"; fi`)
			startSession(t, p, guardSession, tc.cfg)
			if v, _ := p.GetMeta(guardSession, metaLaunchedCommand); v != tc.launched {
				t.Errorf("%s = %q; want %q", metaLaunchedCommand, v, tc.launched)
			}
			if v, _ := p.GetMeta(guardSession, metaAgentProcesses); v != tc.processes {
				t.Errorf("%s = %q; want %q", metaAgentProcesses, v, tc.processes)
			}
			if _, err := os.Stat(filepath.Join(state, "agent_started")); err == nil {
				if _, err := os.Stat(filepath.Join(state, "launch_recorded_before_launch")); err != nil {
					t.Error("the launch facts were not in the sidecar before the agent launch")
				}
			}
		})
	}
}

// The check runs right before each verb that types: once before the prompt,
// and again before the paste that follows the registry round trips. An agent
// that leaves in between gets nothing pasted, and a refusal before the prompt
// issues no verb at all.
func TestDeliveryChecksRightBeforeEachTypingVerb(t *testing.T) {
	const agentless = `{"error":{"code":"agent_not_found","message":"no agent registered for w1:p1"},"id":"cli:agent:prompt"}`
	gone := errors.New("the agent left the pane")
	deliveries := map[string]func(c *client, check typeCheck) error{
		"nudge": func(c *client, check typeCheck) error {
			return c.deliverNudge(context.Background(), "w1:p1", "proceed with the drain", check)
		},
		"startup turn": func(c *client, check typeCheck) error {
			return c.deliverStartupTurn(context.Background(), "w1:p1", "first turn", check)
		},
	}
	for name, deliver := range deliveries {
		t.Run(name+": agent leaves before the paste", func(t *testing.T) {
			bin, trace := registryFake(t, []string{}, agentless)
			calls := 0
			err := deliver(&client{session: "gc-test", bin: bin}, func(context.Context, string) error {
				if calls++; calls > 1 {
					return gone
				}
				return nil
			})
			if !errors.Is(err, gone) || calls != 2 {
				t.Fatalf("delivery = %v after %d checks; want the second check's refusal", err, calls)
			}
			assertPasted(t, trace, false)
		})
		t.Run(name+": refused before the prompt", func(t *testing.T) {
			bin, trace := registryFake(t, []string{}, agentless)
			if err := deliver(&client{session: "gc-test", bin: bin}, func(context.Context, string) error { return gone }); !errors.Is(err, gone) {
				t.Fatalf("delivery = %v; want the check's refusal", err)
			}
			if _, err := os.Stat(trace); !os.IsNotExist(err) {
				b, _ := os.ReadFile(trace)
				t.Fatalf("herdr was called although the check refused:\n%s", b)
			}
		})
	}
}

// A refusal is a *paneNotAgentError whose cause, when the foreground could not
// be read, is herdr's error, and whose text names the session, the pane and
// what runs there.
func TestNudgeRefusalCarriesItsReason(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	startSession(t, p, guardSession, runtime.Config{Command: "claude"})

	setForegroundReply(t, state, `{"error":{"code":"internal_error","message":"process table unavailable"}}`)
	err := p.Nudge(guardSession, runtime.TextContent(guardNudge))
	var refused *paneNotAgentError
	if !errors.As(err, &refused) || herdrErrorCode(err) != "internal_error" {
		t.Fatalf("Nudge = %v; want a *paneNotAgentError wrapping herdr's internal_error", err)
	}

	setForeground(t, state, 100, fgProc(101, "vim", "vim", "notes.md"))
	err = p.Nudge(guardSession, runtime.TextContent(guardNudge))
	assertRefused(t, err, "pane runs vim", "%5", "agent: any of")
}
