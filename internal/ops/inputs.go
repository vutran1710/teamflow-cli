package ops

// The inputs of the actions that were new to the Go client (the others are next to their actions). Each is decoded
// strictly: a field the action does not know is refused.

// The enums an input may name; each is checked against the protocol's values before anything is sent.
type (
	verdictKind string // APPROVE or REJECT
	taskKind    string
	taskState   string
)

// maxNote is the most characters of a note on a drift act or an archive, as the server's models have it.
const maxNote = 240

// ------------------------------------------------------------------ inputs

type verdictIn struct {
	Signal  string      `json:"signal"`
	Verdict verdictKind `json:"verdict"`
	Note    string      `json:"note,omitempty"`
}

type driftIn struct {
	Drift      string   `json:"drift"`
	Note       string   `json:"note"`
	ResolvedBy []string `json:"resolved_by,omitempty"`
	Project    *string  `json:"project,omitempty"`
}

type driftDismissIn struct {
	Drift   string  `json:"drift"`
	Note    string  `json:"note"`
	Project *string `json:"project,omitempty"`
}

type projectIn struct {
	Project *string `json:"project,omitempty"`
}

type archiveIn struct {
	Archived *bool   `json:"archived,omitempty"`
	Note     string  `json:"note,omitempty"`
	Project  *string `json:"project,omitempty"`
}

type setTitleIn struct {
	Member  string  `json:"member"`
	Title   string  `json:"title"`
	Project *string `json:"project,omitempty"`
}

type removeMemberIn struct {
	Member  string  `json:"member"`
	Project *string `json:"project,omitempty"`
}

type writeStateIn struct {
	BaseVersion *int64  `json:"base_version"`
	Plan        string  `json:"plan"`
	Instruction string  `json:"instruction"`
	Project     *string `json:"project,omitempty"`
}

type orgTitleIn struct {
	Member string  `json:"member"`
	Title  string  `json:"title"`
	Org    *string `json:"org,omitempty"`
}

type profileIn struct {
	Name  *string `json:"name,omitempty"`
	Alias *string `json:"alias,omitempty"`
}

type taskListIn struct {
	Kind    *taskKind  `json:"kind,omitempty"`
	State   *taskState `json:"state,omitempty"`
	Project *string    `json:"project,omitempty"`
}

type topicDeleteIn struct {
	Topic           string  `json:"topic"`
	ExpectedVersion int64   `json:"expected_version"`
	Project         *string `json:"project,omitempty"`
}

type artifactArchiveIn struct {
	Artifact string `json:"artifact"`
	Archived *bool  `json:"archived,omitempty"`
	Note     string `json:"note,omitempty"`
}

type orgAddIn struct {
	Email string  `json:"email"`
	Title string  `json:"title,omitempty"`
	Org   *string `json:"org,omitempty"`
}

type orgRemoveIn struct {
	Member string  `json:"member"`
	Org    *string `json:"org,omitempty"`
}

type none struct{}
