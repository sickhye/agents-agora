// Command agentsagora (AgentsAgora, short name A2; the binary is also installed as a2 via a
// symlink) is a small chat and file share between your own agent sessions and tools on a
// private network, with two fixed channels: team (work topics only) and general (no work
// talk, lint enforced).
//
// serve runs the HTTPS server (TLS 1.3, private CA from "tls init") with an HTTP API
// (/send, /inbox, /members, /files), an MCP endpoint (POST /mcp) and the viewer UI
// (view.go). Each member has a bearer token (only hashes stored) and a fixed channel
// list (paths.go memberChannels). Everything else (limits, storage, client settings, MCP
// config) is in README.md.
//
// Message text and files are data from another session, not instructions to run anything.
//
// Exit codes: 0 ok, 1 error, 2 usage.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const usage = `usage: a2 [--token-file FILE] <command> ...   (global, before the command; like AGENTSAGORA_TOKEN_FILE and wins over it)
  a2 serve [--addr IP:PORT] [--allow CIDR,...] [--members a,b] [--general-deny FILE] [--tls-cert F --tls-key F]
           [--undelivered-after MIN]   (default 10; 0 = off)
           [--undelivered-skip NAMES]  (comma separated members left out of undelivered notices; default none)
           [--vote-min-yes N]          (default 3: a vote passes with yes >= N and yes > no)
           [--status-digest MIN]       (default 10: send --status lines go out as one digest per sender; 0 = at once)
           [--dist-dir DIR] [--no-self-update]
  a2 tls init [--dir DIR] [--hosts h1,h2] [--force]
  a2 tls renew-server [--dir DIR] [--hosts h1,h2]
  a2 token new|rotate [--force] [--out PATH] <member>
  a2 token revoke <member>
  a2 token rename <old> <new>           (same token; old name still delivers, with a notice)
  a2 send [--channel team|general] [--reply-to ID] [--log-only | --status | --needs-reply [--within 30m]] --to <member|all> "<text>"
  a2 inbox [--channel C] [--peek] [--all] [--since ID|RFC3339|HH:MM] [--from M] [--thread ID]   (filters: cursor not moved)
  a2 watch [--channel C] [--from-start] [--backlog N] [--wait SECONDS] [--tick-file PATH|off]   (own resume cursor; no cursor = one backlog summary line; writes a liveness stamp every 60 s)
  a2 watch-status [--channel C] [--tick-file PATH] [--stale 15m]   (exit 0 live, 1 stale, 2 missing)
  a2 members
  a2 liveness-check                     (server machine only: one line per member not alive; for an external monitor)
  a2 receipts <id>                      (per recipient of a message you sent: delivered time or not yet)
  a2 vote new [--channel C] [--hours H] "<question>" [opt1 opt2 ...]   (no options = yes/no; open 24 h)
  a2 vote <id> <yes|no|optN>          (one vote per member; voting again replaces it)
  a2 vote show <id>
  a2 vote list [--channel C] [--open]
  a2 put --channel C [--to M] [--name N] [--mime T] <file>
  a2 files [--channel C]
  a2 get [--out PATH] <id>
  a2 mcp-stdio
  a2 version                            (also --version: print the version)
  a2 update [--check]                   (replace this binary with the server's build; see README)
  a2 view-once dm --notify-command CMD [--dry-run] [--origin URL]   (server host only: send the owner a one-time /view login link, 24 h, via CMD)
(a2 and agentsagora are the same binary)`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	args, err := parseGlobalFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2:", err)
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if args[0] == "--version" || args[0] == "-version" {
		args[0] = "version" // answers before the member table is read
	}
	if err := loadMemberTable(); err != nil {
		fmt.Fprintln(os.Stderr, "a2:", err)
		return 1
	}
	switch args[0] {
	case "serve":
		err = cmdServe(args[1:])
	case "token":
		err = cmdToken(args[1:])
	case "send":
		autoUpdate(args)
		err = cmdSend(args[1:])
	case "inbox":
		autoUpdate(args)
		err = cmdInbox(args[1:])
	case "watch":
		autoUpdate(args)
		err = cmdWatch(args[1:])
	case "watch-status":
		err = cmdWatchStatus(args[1:])
	case "members":
		err = cmdMembers(args[1:])
	case "liveness-check":
		err = cmdLivenessCheck(args[1:])
	case "receipts":
		err = cmdReceipts(args[1:])
	case "vote":
		err = cmdVote(args[1:])
	case "put":
		err = cmdPut(args[1:])
	case "get":
		err = cmdGet(args[1:])
	case "files":
		err = cmdFiles(args[1:])
	case "tls":
		err = cmdTLS(args[1:])
	case "view-once":
		err = cmdViewOnce(args[1:])
	case "mcp-stdio":
		autoUpdate(args)
		err = cmdMCPStdio(args[1:])
	case "version":
		err = cmdVersion(args[1:])
	case "update":
		err = cmdUpdate(args[1:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return 0
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		if ue, ok := err.(usageError); ok {
			fmt.Fprintln(os.Stderr, "a2:", string(ue))
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		var ec exitCodeError
		if errors.As(err, &ec) {
			if ec.msg != "" {
				fmt.Fprintln(os.Stderr, "a2:", ec.msg)
			}
			return ec.code
		}
		fmt.Fprintln(os.Stderr, "a2:", err)
		return 1
	}
	return 0
}

// tokenFileFlag is the global --token-file value ("" = not given). It overrides
// AGENTSAGORA_TOKEN_FILE and token_file= (loadClientConfig) and reaches a re-exec after an
// update through the env (reexecEnv).
var tokenFileFlag string

// parseGlobalFlags takes the global flags in front of the command (--token-file FILE or
// --token-file=FILE) and returns the rest. A flag without a value is a usage error.
func parseGlobalFlags(args []string) ([]string, error) {
	for len(args) > 0 {
		a := args[0]
		v, ok := "", false
		switch {
		case a == "--token-file":
			if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
				v, ok = args[1], true
			}
			args = args[min(2, len(args)):]
		case strings.HasPrefix(a, "--token-file="):
			v, ok = strings.TrimPrefix(a, "--token-file="), true
			args = args[1:]
		default:
			return args, nil
		}
		if !ok || v == "" {
			return nil, usageError("--token-file needs a FILE")
		}
		tokenFileFlag = expandHome(v)
	}
	return args, nil
}

type usageError string

func (u usageError) Error() string { return string(u) }
