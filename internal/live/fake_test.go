package live

// A fake Teamflow server for the background sync: JSON-RPC on /rpc (live.ticket, sync.poll, signal.emit,
// key.fetch, member.list) and the live socket on /live, speaking the protocol of the golden live messages
// (protocol/golden/live_message.*): a hello first, a LiveEvents answer (empty, or reset when the cursor is beyond
// the head), then each new event; "ping" answered "pong"; a LiveClosed then the close (1008) on a refusal.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

const (
	P1 = "pj_00000000000000000000000001"
	P2 = "pj_00000000000000000000000002"
	M1 = "mb_00000000000000000000000001"
	M2 = "mb_00000000000000000000000002"
)

type hello struct {
	Project, Ticket string
	Cursor          int64
	At              time.Time
}

type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	events    map[string][]json.RawMessage // per project, in seq order
	seqs      map[string][]int64
	tickets   map[string]string // ticket -> project
	used      map[string]bool   // tickets already presented
	ticketN   int
	ticketErr map[string]string   // project -> the reason live.ticket refuses it with
	closeWith map[string][]string // project -> the next hellos are closed with these reasons ("drop": no LiveClosed)
	refuseWS  bool                // /live answers 426, never upgrades
	silent    bool                // pings go unanswered
	hellos    []hello
	pings     int
	nonPing   []string // text the client sent after the hello that was not "ping"
	conns     map[string]*websocket.Conn
	calls     map[string]int
	polls     []protocol.SyncPoll
	pollHold  time.Duration // a poll with nothing new waits this long (never its full wait)
	// what had happened when the first poll came
	urlsAtFirstPoll, ticketsAtFirstPoll int
	urls                                []string                               // each /live request's URL
	headers                             []http.Header                          // each /live request's headers
	keyFetch                            string                                 // key.fetch refuses with this reason ("": answers)
	liveOff                             bool                                   // live.ticket refuses every project as a server with live sync off does
	pollErr                             map[string]string                      // project -> the reason sync.poll refuses it with
	emitHook                            func(n int, client string) (fail bool) // before signal.emit request n (1-based), outside the lock
	emitN                               int
	clients                             []string // the Teamflow-Client of each signal.emit request
	// the outbox
	emitDelay time.Duration
	order     []string
	results   map[string]bool
	inFlight  atomic.Int32
	maxIn     atomic.Int32
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, events: map[string][]json.RawMessage{}, seqs: map[string][]int64{}, tickets: map[string]string{},
		used: map[string]bool{}, ticketErr: map[string]string{}, pollErr: map[string]string{}, closeWith: map[string][]string{},
		conns: map[string]*websocket.Conn{}, calls: map[string]int{}, results: map[string]bool{},
		pollHold: 20 * time.Millisecond}
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", f.rpc)
	mux.HandleFunc(protocol.LivePath, f.live)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) URL() string { return f.srv.URL }

func (f *fake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fake) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fake) helloList() []hello {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hello(nil), f.hellos...)
}

func (f *fake) hellosFor(project string) []hello {
	var out []hello
	for _, h := range f.helloList() {
		if h.Project == project {
			out = append(out, h)
		}
	}
	return out
}

func (f *fake) head(project string) int64 {
	if s := f.seqs[project]; len(s) > 0 {
		return s[len(s)-1]
	}
	return 0
}

// after is the project's events after cursor (caller holds mu).
func (f *fake) after(project string, cursor int64) ([]json.RawMessage, []int64) {
	var evs []json.RawMessage
	var seqs []int64
	for i, s := range f.seqs[project] {
		if s > cursor {
			evs = append(evs, f.events[project][i])
			seqs = append(seqs, s)
		}
	}
	return evs, seqs
}

// add records events without sending them (what the server holds before a socket opens).
func (f *fake) add(project string, seqs ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range seqs {
		f.events[project] = append(f.events[project], topicEvent(project, s))
		f.seqs[project] = append(f.seqs[project], s)
	}
}

// push records one event and sends it to the project's open socket, as a unit commits.
func (f *fake) push(project string, seq int64, ev json.RawMessage) {
	f.mu.Lock()
	if ev == nil {
		ev = topicEvent(project, seq)
	}
	f.events[project] = append(f.events[project], ev)
	f.seqs[project] = append(f.seqs[project], seq)
	conn := f.conns[project]
	f.mu.Unlock()
	if conn != nil {
		msg := fmt.Sprintf(`{"type": "events", "events": [%s], "cursor": %d}`, ev, seq)
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(msg))
	}
}

// live is the /live socket.
func (f *fake) live(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.urls = append(f.urls, r.URL.String())
	f.headers = append(f.headers, r.Header.Clone())
	refuse := f.refuseWS
	f.mu.Unlock()
	if refuse {
		http.Error(w, "expected a WebSocket upgrade", http.StatusUpgradeRequired)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	project := r.URL.Query().Get("project")
	closeWith := func(reason, message string) {
		b, _ := json.Marshal(map[string]string{"type": "closed", "reason": reason, "message": message})
		_ = conn.Write(ctx, websocket.MessageText, b)
		_ = conn.Close(websocket.StatusCode(protocol.LiveCloseCode), "closed")
	}
	var h protocol.LiveHello
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText && string(data) == protocol.LivePing {
			f.pong(ctx, conn)
			continue
		}
		if err := json.Unmarshal(data, &h); err != nil || h.Type != "hello" || h.Ticket == "" {
			closeWith("SCHEMA_VIOLATION", `the first message must be a hello: {"ticket": "...", "cursor": n}`)
			return
		}
		break
	}
	f.mu.Lock()
	f.hellos = append(f.hellos, hello{Project: project, Ticket: h.Ticket, Cursor: h.Cursor, At: time.Now()})
	good := f.tickets[h.Ticket] == project && !f.used[h.Ticket]
	f.used[h.Ticket] = true
	var reason string
	if q := f.closeWith[project]; len(q) > 0 {
		reason, f.closeWith[project] = q[0], q[1:]
	}
	f.mu.Unlock()
	switch {
	case !good:
		closeWith("NOT_ALLOWED", "this ticket has expired or is not for this project: "+h.Ticket)
		return
	case reason == "drop": // the connection goes, no LiveClosed (a network drop)
		return
	case reason != "":
		closeWith(reason, "closed by the test (ticket "+h.Ticket+")")
		return
	}
	f.mu.Lock()
	head := f.head(project)
	var first string
	if h.Cursor > head {
		first = fmt.Sprintf(`{"type": "events", "events": [], "cursor": %d, "reset": true}`, head)
	} else {
		evs, seqs := f.after(project, h.Cursor)
		cursor := h.Cursor
		if len(seqs) > 0 {
			cursor = seqs[len(seqs)-1]
		}
		raws := make([]string, len(evs))
		for i, e := range evs {
			raws[i] = string(e)
		}
		first = fmt.Sprintf(`{"type": "events", "events": [%s], "cursor": %d}`, strings.Join(raws, ", "), cursor)
	}
	// Sent while holding mu, so a push can't overtake the catch-up.
	err = conn.Write(ctx, websocket.MessageText, []byte(first))
	if err == nil && h.Cursor <= head {
		f.conns[project] = conn
	}
	f.mu.Unlock()
	if err != nil {
		return
	}
	defer f.set(func() {
		if f.conns[project] == conn {
			delete(f.conns, project)
		}
	})
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText && string(data) == protocol.LivePing {
			f.pong(ctx, conn)
			continue
		}
		f.set(func() { f.nonPing = append(f.nonPing, string(data)) }) // ignored after the hello
	}
}

func (f *fake) pong(ctx context.Context, conn *websocket.Conn) {
	f.mu.Lock()
	f.pings++
	silent := f.silent
	f.mu.Unlock()
	if !silent {
		_ = conn.Write(ctx, websocket.MessageText, []byte(protocol.LivePong))
	}
}

// kick closes the project's socket from the server side with no LiveClosed (the network dropped it).
func (f *fake) kick(project string) {
	f.mu.Lock()
	conn := f.conns[project]
	delete(f.conns, project)
	f.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// closeLive sends the project's open socket a LiveClosed with reason, then the close (1008).
func (f *fake) closeLive(project, reason string) {
	f.mu.Lock()
	conn := f.conns[project]
	delete(f.conns, project)
	f.mu.Unlock()
	if conn != nil {
		b, _ := json.Marshal(map[string]string{"type": "closed", "reason": reason, "message": "closed by the test"})
		_ = conn.Write(context.Background(), websocket.MessageText, b)
		_ = conn.Close(websocket.StatusCode(protocol.LiveCloseCode), "closed")
	}
}

// sendRaw sends text as it is to the project's open socket.
func (f *fake) sendRaw(project, text string) {
	f.mu.Lock()
	conn := f.conns[project]
	f.mu.Unlock()
	if conn != nil {
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(text))
	}
}

// liveOn says whether the project has an open socket that got its first answer.
func (f *fake) liveOn(project string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[project] != nil
}

func rpcErr(reason, msg string) *protocol.RpcError {
	return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: msg, Data: map[string]any{"reason": reason}}
}

func (f *fake) rpc(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	batch := strings.HasPrefix(strings.TrimSpace(string(body)), "[")
	var reqs []protocol.Request
	if batch {
		_ = json.Unmarshal(body, &reqs)
	} else {
		var one protocol.Request
		_ = json.Unmarshal(body, &one)
		reqs = []protocol.Request{one}
	}
	if len(reqs) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if reqs[0].Method == "signal.emit" {
		if in := f.inFlight.Add(1); in > f.maxIn.Load() {
			f.maxIn.Store(in)
		}
		defer f.inFlight.Add(-1)
		f.mu.Lock()
		d, hook := f.emitDelay, f.emitHook
		f.emitN++
		n, client := f.emitN, r.Header.Get(protocol.ClientHeader)
		f.clients = append(f.clients, client)
		f.mu.Unlock()
		if hook != nil && hook(n, client) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		time.Sleep(d)
	}
	var outs []protocol.Response
	for _, q := range reqs {
		outs = append(outs, f.answer(r, q))
	}
	w.Header().Set("Content-Type", "application/json")
	if !batch {
		_ = json.NewEncoder(w).Encode(outs[0])
		return
	}
	_ = json.NewEncoder(w).Encode(outs)
}

func (f *fake) answer(r *http.Request, q protocol.Request) protocol.Response {
	out := protocol.Response{JSONRPC: "2.0", ID: q.ID}
	f.mu.Lock()
	f.calls[q.Method]++
	f.mu.Unlock()
	switch q.Method {
	case "live.ticket":
		var p protocol.ProjectRef
		_ = json.Unmarshal(q.Params, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.liveOff {
			out.Error = rpcErr("NOT_ALLOWED", "live sync is not set up on this server; use sync.poll")
			return out
		}
		if reason := f.ticketErr[p.Project]; reason != "" {
			out.Error = rpcErr(reason, "refused by the test")
			return out
		}
		f.ticketN++
		ticket := fmt.Sprintf("tkt%dSECRETTICKETVALUE%d", f.ticketN, time.Now().UnixNano())
		f.tickets[ticket] = p.Project
		b, _ := json.Marshal(protocol.LiveTicket{Ticket: ticket, Org: "og_00000000000000000000000001", Project: p.Project,
			ExpiresAt: time.Now().Add(protocol.LiveTicketSeconds * time.Second).UTC().Format(time.RFC3339)})
		out.Result = b
	case "sync.poll":
		var p protocol.SyncPoll
		_ = json.Unmarshal(q.Params, &p)
		f.mu.Lock()
		for proj := range p.Cursors {
			if reason := f.pollErr[proj]; reason != "" {
				f.mu.Unlock()
				out.Error = rpcErr(reason, "refused by the test")
				return out
			}
		}
		if len(f.polls) == 0 {
			f.urlsAtFirstPoll, f.ticketsAtFirstPoll = len(f.urls), f.calls["live.ticket"]
		}
		f.polls = append(f.polls, p)
		hold := f.pollHold
		f.mu.Unlock()
		if len(p.Cursors) == 0 { // no cursors: every project from 0 (sync.poll.2)
			p.Cursors = map[string]int64{}
			f.mu.Lock()
			for proj := range f.seqs {
				p.Cursors[proj] = 0
			}
			f.mu.Unlock()
		}
		res := protocol.SyncBatch{Events: []protocol.SyncEvent{}, Cursors: map[string]int64{}, Reset: []string{}}
		var raws []string
		for i := 0; i < 2; i++ {
			f.mu.Lock()
			raws = raws[:0]
			for proj, cur := range p.Cursors {
				if _, known := f.seqs[proj]; !known {
					continue
				}
				if cur > f.head(proj) {
					res.Reset = append(res.Reset, proj)
					res.Cursors[proj] = f.head(proj)
					continue
				}
				evs, seqs := f.after(proj, cur)
				for _, e := range evs {
					raws = append(raws, string(e))
				}
				res.Cursors[proj] = cur
				if len(seqs) > 0 {
					res.Cursors[proj] = seqs[len(seqs)-1]
				}
			}
			f.mu.Unlock()
			if len(raws) > 0 || len(res.Reset) > 0 || p.Wait == 0 || i == 1 {
				break
			}
			select { // held, as a long poll is, though never its full wait here
			case <-time.After(hold):
			case <-r.Context().Done():
			}
		}
		cur, _ := json.Marshal(res.Cursors)
		reset, _ := json.Marshal(res.Reset)
		out.Result = json.RawMessage(fmt.Sprintf(`{"events": [%s], "cursors": %s, "reset": %s}`,
			strings.Join(raws, ", "), cur, reset))
	case "signal.emit":
		var p struct {
			Signal struct {
				ID string `json:"id"`
			} `json:"signal"`
		}
		_ = json.Unmarshal(q.Params, &p)
		f.mu.Lock()
		f.order = append(f.order, p.Signal.ID)
		f.results[p.Signal.ID] = true
		f.mu.Unlock()
		out.Result = json.RawMessage(fmt.Sprintf(`{"signal": %q, "accepted": true, "message": "", "state": "IN_FORCE"}`, p.Signal.ID))
	case "member.list":
		out.Result = json.RawMessage(`{"members": []}`)
	case "key.fetch":
		f.mu.Lock()
		reason := f.keyFetch
		f.mu.Unlock()
		if reason != "" {
			out.Error = rpcErr(reason, "refused by the test")
			return out
		}
		var p protocol.KeyFetch
		_ = json.Unmarshal(q.Params, &p)
		out.Result = json.RawMessage(`{"project": "` + p.Project + `", "key": "tfk_minted_secret_key", "member": "` + M1 +
			`", "org": "og_00000000000000000000000001", "name": "Pricing"}`)
	default:
		out.Error = &protocol.RpcError{Code: protocol.CodeMethodNotFound, Message: "no such method " + q.Method}
	}
	return out
}

// topicEvent is a topic.updated event at seq: a topic named for its seq.
func topicEvent(project string, seq int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"project": %q, "seq": %d, "at": "2026-01-15T09:00:00Z", "type": "topic.updated", `+
		`"topic": {"id": "tp_%s%024d", "project": %q, "name": "T%d", "description": "d", "closed": false, "system": false, `+
		`"created_at": "2026-01-15T09:00:00Z", "updated_at": "2026-01-15T09:00:00Z"}}`, project, seq, project[len(project)-2:], seq, project, seq))
}

// approvalEvent is a signal.accepted at seq asking approver for approval.
func approvalEvent(project string, seq int64, id, approver string) json.RawMessage {
	due := "2026-12-31"
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = id, project, "tp_00000000000000000000000001", protocol.SignalKindDecision, "Round prices"
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points = "key_points.v1", "All prices round", []string{"p"}
	sig.Body = body
	sig.RequestForApproval = &protocol.ApprovalRequest{Approvers: []string{approver}, Required: 1, Due: &due}
	st := protocol.NewStoredSignal()
	st.Signal, st.Author.Member, st.Author.Actor = sig, M2, protocol.ActorAgent
	st.Seq, st.Chain, st.ChainVersion, st.State = seq, id, 1, protocol.SignalStatePending
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal, ev.At = project, seq, "signal.accepted", st, "2026-01-15T09:00:00Z"
	b, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	return b
}
