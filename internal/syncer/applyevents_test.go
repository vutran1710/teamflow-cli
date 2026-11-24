package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// Every golden event (and a variant from a newer server) gives its project and seq.
func TestPositionOfEveryEvent(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "protocol", "golden", "event.*.json"))
	if len(files) == 0 {
		t.Fatal("no golden events")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var g struct {
			Event json.RawMessage `json:"event"`
		}
		must(t, json.Unmarshal(b, &g))
		var want struct {
			Project string `json:"project"`
			Seq     int64  `json:"seq"`
		}
		must(t, json.Unmarshal(g.Event, &want))
		ev, err := protocol.DecodeSyncEvent(g.Event)
		must(t, err)
		if p, s := Position(ev); p != want.Project || s != want.Seq || s == 0 {
			t.Fatalf("%s: %s %d, want %s %d", filepath.Base(f), p, s, want.Project, want.Seq)
		}
	}
	ev, err := protocol.DecodeSyncEvent(json.RawMessage(`{"type": "future.thing", "project": "pj_9", "seq": 7}`))
	must(t, err)
	if p, s := Position(ev); p != "pj_9" || s != 7 {
		t.Fatalf("unknown: %s %d", p, s)
	}
}

// ApplyEvents applies each event once: one at or below the project's cursor (brought in by another sync first, or
// sent again) is skipped; holes are fine; another project's cursor is its own.
func TestApplyEventsOnce(t *testing.T) {
	st := openStore(t)
	topic := func(p string, seq int64) protocol.SyncEvent {
		ev, err := protocol.DecodeSyncEvent(json.RawMessage(strings.NewReplacer("P", p, "N", strings.Repeat("0", 25)+string(rune('0'+seq))).Replace(
			`{"project": "P", "seq": ` + string(rune('0'+seq)) + `, "at": "2026-01-15T09:00:00Z", "type": "topic.updated", "topic": {"id": "tp_N", "project": "P", "name": "T", "description": "", "closed": false, "system": false, "created_at": "2026-01-15T09:00:00Z", "updated_at": "2026-01-15T09:00:00Z"}}`)))
		must(t, err)
		return ev
	}
	var got []string
	done := func(p string, s int64) { got = append(got, p+":"+string(rune('0'+s))) }
	must(t, st.Tx(func(tx *store.Tx) error { return tx.SetCursor("pj_a", 3) }))
	must(t, st.Tx(func(tx *store.Tx) error {
		n, err := ApplyEvents(tx, []protocol.SyncEvent{topic("pj_a", 2), topic("pj_a", 3), topic("pj_a", 5), topic("pj_b", 1),
			topic("pj_a", 5), topic("pj_a", 9)}, time.Now(), done)
		if n != 3 {
			t.Fatalf("applied %d", n)
		}
		return err
	}))
	if strings.Join(got, ",") != "pj_a:5,pj_b:1,pj_a:9" {
		t.Fatalf("applied %v", got)
	}
}
