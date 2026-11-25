package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// One project offline (its org's server answering 503) or failing (a refusal) never keeps the sync from the
// machine's other projects: each is pushed and pulled, and the answer is the combined state (false when any was
// offline). Sad paths: the first project offline, the first failing with a JSON-RPC error, a queued signal on the
// project behind it still sent, and the offline project's key kept.
func TestSyncGoesOnPastAFailingProject(t *testing.T) {
	const P2 = "pj_00000000000000000000000002"
	for _, mode := range []string{"offline", "failing"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			hits := map[string][]string{} // key -> methods
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				key := r.Header.Get("Authorization")
				var reqs []protocol.Request
				if json.Unmarshal(body, &reqs) != nil {
					var one protocol.Request
					_ = json.Unmarshal(body, &one)
					reqs = []protocol.Request{one}
				}
				mu.Lock()
				for _, q := range reqs {
					hits[key] = append(hits[key], q.Method)
				}
				mu.Unlock()
				if key == "Bearer tfk_p1" {
					if mode == "offline" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 0, "error": {"code": -32000, "message": "boom", "data": {"reason": "INVALID"}}}`))
					return
				}
				outs := []protocol.Response{}
				for _, q := range reqs {
					out := protocol.Response{JSONRPC: "2.0", ID: q.ID}
					switch q.Method {
					case "sync.poll":
						out.Result = json.RawMessage(`{"events": [], "cursors": {}, "reset": []}`)
					case "signal.emit":
						var p struct {
							Signal struct {
								ID string `json:"id"`
							} `json:"signal"`
						}
						_ = json.Unmarshal(q.Params, &p)
						out.Result = json.RawMessage(`{"signal": "` + p.Signal.ID + `", "accepted": true, "message": "", "state": "IN_FORCE"}`)
					default:
						out.Result = json.RawMessage(`{"members": []}`)
					}
					outs = append(outs, out)
				}
				if len(body) > 0 && body[0] == '[' {
					_ = json.NewEncoder(w).Encode(outs)
					return
				}
				_ = json.NewEncoder(w).Encode(outs[0])
			}))
			t.Cleanup(srv.Close)
			c := open(t, &config.Config{Server: srv.URL, AuthKey: "tfa_example", Email: "lan@acme.test"})
			b := store.Binding(srv.URL, "lan@acme.test")
			must(t, c.St.PutProjectKey(b, P, "tfk_p1", tM1, "og_x", "Pricing", time.Now()))
			must(t, c.St.PutProjectKey(b, P2, "tfk_p2", tM1, "og_x", "Ops", time.Now()))
			e := protocol.NewSignalEmit()
			e.ClientSession, e.Actor, e.ProjectVersion = "s1", protocol.ActorAgent, 1
			e.Signal = protocol.NewSignal()
			e.Signal.ID, e.Signal.Project, e.Signal.Topic, e.Signal.Kind, e.Signal.Title = "sg_p2note", P2, tT1, protocol.SignalKindNote, "n"
			e.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "why"}
			raw, _ := json.Marshal(e)
			must(t, c.St.SpoolAdd(e.Signal.ID, "s1", store.OutboxOf(b, P2), string(raw), time.Now()))

			online := c.Sync(context.Background(), time.Second, "")
			if online != (mode != "offline") {
				t.Fatalf("combined state %v", online)
			}
			mu.Lock()
			p1, p2 := hits["Bearer tfk_p1"], hits["Bearer tfk_p2"]
			mu.Unlock()
			if len(p1) == 0 {
				t.Fatal("the first project was never tried")
			}
			if !contains(p2, "sync.poll") || !contains(p2, "signal.emit") {
				t.Fatalf("the project behind it was skipped: %v", p2)
			}
			if items, _ := c.St.Spooled(store.OutboxOf(b, P2)); len(items) != 0 {
				t.Fatalf("its queued signal was not sent: %v", items)
			}
			if _, _, _, _, ok := c.St.ProjectKey(b, P); !ok && mode == "offline" {
				t.Fatal("an offline project's key was dropped")
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
