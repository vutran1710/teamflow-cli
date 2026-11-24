package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Goldens are picked by what they hold, never by their file number: the export numbers each event type's goldens
// in the order the server tests record them, and a new server test can shift every number after it.

// shape is one kind of golden event a test needs: its event type and what it must hold.
type shape struct {
	typ  string
	want func(protocol.SyncEvent) bool
}

var shapes = map[string]shape{
	"signal in force": {"signal.accepted", func(ev protocol.SyncEvent) bool {
		s, ok := ev.(protocol.SignalAccepted)
		_, kp := s.Signal.Signal.Body.(protocol.KeyPointsBody)
		return ok && kp && s.Signal.State == protocol.SignalStateInForce && s.Signal.Signal.RequestForApproval == nil &&
			s.Signal.Signal.Title == "Round prices to 0.05" && s.Seq == 5
	}},
	"signal asking approval": {"signal.accepted", func(ev protocol.SyncEvent) bool {
		s, ok := ev.(protocol.SignalAccepted)
		return ok && s.Signal.State == protocol.SignalStatePending && s.Signal.Signal.RequestForApproval != nil
	}},
	"artifact signal": {"signal.accepted", func(ev protocol.SyncEvent) bool {
		s, ok := ev.(protocol.SignalAccepted)
		_, art := s.Signal.Signal.Body.(protocol.ArtifactBody)
		return ok && art
	}},
	"superseded, another in force": {"signal.state_changed", func(ev protocol.SyncEvent) bool {
		s, ok := ev.(protocol.SignalStateChanged)
		return ok && s.New == protocol.SignalStateSuperseded && s.InForce != nil
	}},
	"rejected": {"signal.state_changed", func(ev protocol.SyncEvent) bool {
		s, ok := ev.(protocol.SignalStateChanged)
		return ok && s.New == protocol.SignalStateRejected
	}},
	"project and state": {"project.updated", func(ev protocol.SyncEvent) bool {
		p, ok := ev.(protocol.ProjectUpdated)
		return ok && p.Updated != nil && p.State != nil && len(p.Updated.Members) == 2
	}},
	"project only": {"project.updated", func(ev protocol.SyncEvent) bool {
		p, ok := ev.(protocol.ProjectUpdated)
		return ok && p.Updated != nil && p.State == nil && len(p.Updated.Members) == 3
	}},
	"state only": {"project.updated", func(ev protocol.SyncEvent) bool {
		p, ok := ev.(protocol.ProjectUpdated)
		return ok && p.Updated == nil && p.State != nil
	}},
	"system topic": {"topic.updated", func(ev protocol.SyncEvent) bool {
		tp, ok := ev.(protocol.TopicUpdatedEvent)
		return ok && tp.Topic.System
	}},
	"topic deleted with signals and a drift": {"topic.deleted", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.TopicDeletedEvent)
		return ok && len(d.Signals) == 2 && len(d.Drifts) == 1
	}},
	"empty topic deleted": {"topic.deleted", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.TopicDeletedEvent)
		return ok && len(d.Signals) == 0 && len(d.Drifts) == 0
	}},
	"plan conflict found": {"drift.found", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DriftFound)
		return ok && d.Drift.Kind == protocol.DriftKindPlanConflict
	}},
	"conflict of two signals found": {"drift.found", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DriftFound)
		return ok && d.Drift.Kind == protocol.DriftKindSignalConflict && len(d.Drift.Signals) == 2
	}},
	"plan conflict resolved": {"drift.resolved", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DriftResolved)
		return ok && d.Drift.Status == protocol.DriftStatusResolved && d.Drift.Kind == protocol.DriftKindPlanConflict
	}},
	"drift dismissed": {"drift.resolved", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DriftResolved)
		return ok && d.Drift.Status == protocol.DriftStatusDismissed
	}},
	"artifact archived": {"artifact.archived", func(ev protocol.SyncEvent) bool {
		a, ok := ev.(protocol.ArtifactArchivedEvent)
		return ok && a.Archived
	}},
	// A1 "Draft rounding rules" is M2's, in T1; A2 has no owner.
	"first deliverables of two milestones": {"deliverables.published", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DeliverablesPublished)
		return ok && d.Deliverables.Version == 1 && len(d.Deliverables.Milestones) == 2
	}},
	"second deliverables": {"deliverables.published", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DeliverablesPublished)
		return ok && d.Deliverables.Version == 2
	}},
	"deliverables archived": {"deliverables.archived", func(ev protocol.SyncEvent) bool {
		d, ok := ev.(protocol.DeliverablesArchived)
		return ok && d.Archived
	}},
	"deliverables deleted": {"deliverables.deleted", func(ev protocol.SyncEvent) bool {
		_, ok := ev.(protocol.DeliverablesDeleted)
		return ok
	}},
}

// goldenEvents are every golden of an event type, in file order: (raw event, decoded event).
func goldenEvents(t *testing.T, typ string) ([]json.RawMessage, []protocol.SyncEvent) {
	t.Helper()
	dir := filepath.Join("..", "..", "protocol", "golden")
	files, err := filepath.Glob(filepath.Join(dir, "event."+typ+".*.json"))
	must(t, err)
	num := func(f string) int {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "event."+typ+"."), ".json"))
		return n
	}
	sort.Slice(files, func(i, j int) bool { return num(files[i]) < num(files[j]) })
	var raws []json.RawMessage
	var evs []protocol.SyncEvent
	for _, f := range files {
		if num(f) == 0 { // another type whose name starts with this one
			continue
		}
		b, err := os.ReadFile(f)
		must(t, err)
		var g struct {
			Event json.RawMessage `json:"event"`
		}
		must(t, json.Unmarshal(b, &g))
		ev, err := protocol.DecodeSyncEvent(g.Event)
		must(t, err)
		raws, evs = append(raws, g.Event), append(evs, ev)
	}
	return raws, evs
}

// golden is the first golden event of the named shape, as the server sent it.
func golden(t *testing.T, name string) json.RawMessage {
	t.Helper()
	sh, ok := shapes[name]
	if !ok {
		t.Fatalf("no golden shape %q", name)
	}
	raws, evs := goldenEvents(t, sh.typ)
	for i, ev := range evs {
		if sh.want(ev) {
			return raws[i]
		}
	}
	t.Fatalf("no golden %s event is %q: re-export the protocol, or the server tests changed", sh.typ, name)
	return nil
}

// event is golden, decoded.
func event(t *testing.T, name string) protocol.SyncEvent {
	t.Helper()
	ev, err := protocol.DecodeSyncEvent(golden(t, name))
	must(t, err)
	return ev
}
