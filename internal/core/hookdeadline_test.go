package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// pagingServer answers every call after delay; sync.poll always says the project was reset (so a pull goes on page
// after page, up to MaxPages): each request is quick enough on its own, the whole sync is not.
func pagingServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q protocol.Request
		_ = json.Unmarshal(body, &q)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out := protocol.Response{JSONRPC: "2.0", ID: q.ID}
		switch q.Method {
		case "sync.poll":
			out.Result = json.RawMessage(`{"events": [], "cursors": {"` + P + `": 0}, "reset": ["` + P + `"]}`)
		case "member.list":
			out.Result = json.RawMessage(`{"members": []}`)
		default:
			out.Result = json.RawMessage(`{}`)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The hooks that sync before answering (the prompt hook, the turn's end, the prompt hook of agents that cannot
// speak there, the first full context, session end) are bounded as a whole by the hook deadline, as the hook
// before a tool is: a slow server that answers each request in time but never ends the pull is cut off, and the
// hook answers from the replica, never failing the step. Sad paths: a slow server, a pull that never ends, a
// session's first prompt (the full context), a session ending with something queued.
func TestHookSyncsHaveAnOverallDeadline(t *testing.T) {
	old := hookDeadline
	hookDeadline = 600 * time.Millisecond
	t.Cleanup(func() { hookDeadline = old })
	limit := hookDeadline + 500*time.Millisecond
	srv := pagingServer(t, 150*time.Millisecond)
	ctx := context.Background()
	timed := func(name string, f func() error) {
		t.Helper()
		start := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s failed the step: %v", name, err)
		}
		if d := time.Since(start); d > limit {
			t.Fatalf("%s took %v (limit %v)", name, d, limit)
		}
	}
	c, _ := midTurnCore(t, srv.URL)
	timed("Full", func() error { _, err := c.Full(ctx, "s1"); return err })
	timed("Prompt", func() error { _, err := c.Prompt(ctx, "s1", "agent", 0, "", "go on"); return err })
	timed("AfterTurn", func() error { c.AfterTurn(ctx, "s1"); return nil })
	timed("BeforePrompt", func() error { return c.BeforePrompt(ctx, "s1", "agent", 0, "") })
	queueNotes(t, c, 0, 1)
	timed("End", func() error { return c.End(ctx, "s1") })
}
