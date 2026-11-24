package cli

import (
	"errors"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// ------------------------------------------------------------------ stop

var stopCmd = &command{name: "stop", help: "Stop sharing the current agent session.", nargs: exactly(0), run: runStop}

func runStop(r *inv, _ *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	sess, ok := r.session()
	if !ok {
		return r.fail(noSession)
	}
	text, err := c.Stop(sess.ID)
	if err != nil {
		return r.failErr(err)
	}
	r.println(text)
	return 0
}

// ------------------------------------------------------------------ approve, reject

var approveCmd = &command{
	name: "approve", args: "<sg_id> [note]",
	help:  "Approve a PENDING signal you were asked to approve. Through your agent, only on your own explicit words.",
	nargs: atLeast(1),
	run: func(r *inv, a *parsed) int {
		return r.verdict(protocol.SignalKindApprove, a.pos[0], strings.Join(a.pos[1:], " "))
	},
}

var rejectCmd = &command{
	name: "reject", args: "<sg_id> <note>",
	help:  "Reject a PENDING signal you were asked to approve, saying why.",
	nargs: atLeast(2),
	run: func(r *inv, a *parsed) int {
		return r.verdict(protocol.SignalKindReject, a.pos[0], strings.Join(a.pos[1:], " "))
	},
}

// verdict records the person's APPROVE or REJECT: their own however it is sent (actor MEMBER), with the agent
// session's id when it runs inside one. It is sent alone.
func (r *inv) verdict(kind protocol.SignalKind, signal, note string) int {
	if kind == protocol.SignalKindReject && strings.TrimSpace(note) == "" {
		return r.fail("say why you reject it: reject " + signal + " <why>")
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	sid := ""
	if sess, ok := r.session(); ok {
		sid = sess.ID
	}
	res, err := c.Verdict(r.ctx, sid, signal, kind, note)
	if err != nil {
		return r.failErr(err)
	}
	if !res.Accepted {
		reason := ""
		if res.Reason != nil {
			reason = string(*res.Reason)
		}
		return r.fail("refused: " + reason + " " + res.Message)
	}
	done := "approved"
	if kind == protocol.SignalKindReject {
		done = "rejected"
	}
	r.printf("%s %s\n", done, signal)
	return 0
}

// ------------------------------------------------------------------ team: behind the /teamf command

var teamCmd = &command{
	name: "team", args: "project [<project> [topic …]] | stop | status | approvals [--all] | approve <sg_id> [note] | reject <sg_id> <note>",
	help: "Behind an agent's Teamflow command (/teamf): choose the session's project and topics, stop, status, " +
		"approval requests, approve or reject.",
	raw: true, run: runTeam,
}

const teamUsage = "usage: project [<project> [topic …]] | stop | status | approvals [--all] | approve <sg_id> [note] | reject <sg_id> <note>"

func runTeam(r *inv, a *parsed) int {
	args := a.pos
	if len(args) > 0 && args[0] == "--relay" { // typed by the person as the agent's Teamflow command
		r.relay, args = true, args[1:]
	}
	if len(args) == 1 && strings.ContainsAny(args[0], " \t\"'\\") { // the command's arguments arrive as one word
		split, err := shellSplit(args[0])
		if err != nil {
			return r.fail(err.Error())
		}
		args = split
	}
	sub, rest := "status", []string(nil)
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch {
	case sub == "project" && len(rest) == 0:
		return r.projectMenu()
	case sub == "project":
		var topics []string
		for _, t := range rest[1:] {
			if t != "--all" {
				topics = append(topics, t)
			}
		}
		return r.join(rest[0], topics)
	case sub == "stop" && len(rest) == 0:
		return runStop(r, nil)
	case sub == "status" && len(rest) == 0:
		return runStatus(r, nil)
	case sub == "approvals" && onlyAll(rest):
		return r.approvals(len(rest) > 0)
	case sub == "approve" && len(rest) >= 1:
		return r.verdict(protocol.SignalKindApprove, rest[0], strings.Join(rest[1:], " "))
	case sub == "reject" && len(rest) >= 2:
		return r.verdict(protocol.SignalKindReject, rest[0], strings.Join(rest[1:], " "))
	}
	return r.fail(teamUsage)
}

func onlyAll(xs []string) bool {
	for _, x := range xs {
		if x != "--all" {
			return false
		}
	}
	return true
}

func (r *inv) projectMenu() int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	sess, ok := r.session()
	if !ok {
		return r.fail(noSession)
	}
	text, err := c.ProjectMenu(r.ctx, sess.ID)
	if err != nil {
		return r.failErr(err)
	}
	r.println(text)
	return 0
}

func (r *inv) join(project string, topics []string) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	sess, ok := r.session()
	if !ok {
		return r.fail(noSession)
	}
	text, err := c.Join(r.ctx, sess.ID, project, topics)
	if err != nil {
		return r.fail("not joined: " + errText(err))
	}
	r.println(text)
	return 0
}

func (r *inv) approvals(all bool) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	sess, ok := r.session()
	if !ok {
		return r.fail(noSession)
	}
	text, err := c.ApprovalsQueue(r.ctx, sess.ID, all)
	if err != nil {
		return r.failErr(err)
	}
	r.println(text)
	return 0
}

// shellSplit splits a line as a POSIX shell would (Python's shlex.split): quotes group, a backslash escapes
// outside single quotes. An unterminated quote or a trailing backslash is refused, as shlex refuses it.
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote, esc := false, rune(0), false
	for _, c := range s {
		switch {
		case esc:
			cur.WriteRune(c)
			esc, in = false, true
		case c == '\\' && quote != '\'':
			esc, in = true, true
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	switch {
	case quote != 0:
		return nil, errors.New("No closing quotation")
	case esc:
		return nil, errors.New("No escaped character")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}
