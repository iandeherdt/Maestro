package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

const boardOrg = "ian-org"

type fakeCard struct {
	status   string
	worker   string
	archived bool
	kind     string // "", "draft", "pr" or "elsewhere"
}

// fakeBoard is a Project with a Status and a Worker field.
type fakeBoard struct {
	number  int
	columns []string
	cards   map[int]*fakeCard // issue number -> card
	extra   []*fakeCard       // cards that are not issues of the repository
	closed  bool
	errors  map[string]string // part of a query -> error type to answer with
	// listStatus, when set for an issue, is what the card list reports
	// instead of the current column.
	listStatus map[int]string
	moves      int
	automation []string
	workers    []string
	noWorkers  bool
}

func newBoardHub(t *testing.T) *hub {
	h := newHub(t)
	h.board = &fakeBoard{number: 7, cards: map[int]*fakeCard{}, errors: map[string]string{}, listStatus: map[int]string{},
		workers: []string{"linux", "Mac"},
		columns: []string{"Backlog", "Todo", "In Progress", "Human Review", "Rework", "Blocked", "Done"}}
	return h
}

// card puts an issue on the board.
func (h *hub) card(n int, author, status, worker string) *fakeIssue {
	fi := h.add(n, author, "")
	h.board.cards[n] = &fakeCard{status: status, worker: worker}
	return fi
}

func optionID(name string) string {
	return "opt_" + strings.ReplaceAll(strings.ToLower(name), " ", "_")
}

func (h *hub) gqlIssue(fi *fakeIssue) map[string]any {
	w := h.wire(fi)
	labels := []any{}
	for _, l := range fi.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]any{
		"__typename": "Issue", "id": fi.nodeID, "number": fi.number, "title": fi.title, "body": fi.body,
		"state": strings.ToUpper(fi.state), "url": w["html_url"], "createdAt": w["created_at"], "updatedAt": w["updated_at"],
		"author": map[string]string{"login": fi.author}, "assignees": map[string]any{"nodes": []any{}},
		"labels": map[string]any{"nodes": labels}, "repository": map[string]string{"nameWithOwner": owner + "/app"},
	}
}

func (h *hub) gqlCard(n int, c *fakeCard, status string) map[string]any {
	m := map[string]any{"id": fmt.Sprintf("PVTI_%d", n), "isArchived": c.archived, "status": nil, "worker": nil,
		"project": map[string]any{"number": h.board.number, "owner": map[string]string{"__typename": "Organization", "login": boardOrg}}}
	if status != "" {
		m["status"] = map[string]string{"name": status}
	}
	if c.worker != "" {
		m["worker"] = map[string]string{"name": c.worker}
	}
	return m
}

func (h *hub) graphql(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	b := h.board
	answer := func(data any) { json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	fail := func(kind string) {
		json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []map[string]string{{"type": kind, "message": "Could not\x07 resolve @someone"}}})
	}
	for part, kind := range b.errors {
		if strings.Contains(in.Query, part) {
			fail(kind)
			return
		}
	}
	str := func(k string) string { s, _ := in.Variables[k].(string); return s }
	num := func(k string) int { f, _ := in.Variables[k].(float64); return int(f) }
	onBoard := strings.EqualFold(str("owner"), boardOrg) && num("number") == b.number
	wrap := func(project any) any {
		return map[string]any{"repositoryOwner": map[string]any{"projectV2": project}}
	}
	switch {
	case strings.Contains(in.Query, "updateProjectV2ItemFieldValue"):
		h.requests = append(h.requests, "MOVE")
		n, _ := strconv.Atoi(strings.TrimPrefix(str("item"), "PVTI_"))
		c := b.cards[n]
		if c != nil && str("project") == "PVT_board" && str("field") == "F_worker" {
			for _, name := range b.workers {
				if optionID(name) == str("option") {
					c.worker = name
					answer(map[string]any{"updateProjectV2ItemFieldValue": map[string]any{"projectV2Item": map[string]string{"id": str("item")}}})
					return
				}
			}
			fail("UNPROCESSABLE")
			return
		}
		if c == nil || str("project") != "PVT_board" || str("field") != "F_status" {
			fail("NOT_FOUND")
			return
		}
		for _, col := range b.columns {
			if optionID(col) == str("option") {
				c.status = col
				b.moves++
				answer(map[string]any{"updateProjectV2ItemFieldValue": map[string]any{"projectV2Item": map[string]string{"id": str("item")}}})
				return
			}
		}
		fail("UNPROCESSABLE")
	case strings.Contains(in.Query, "addProjectV2ItemById"):
		h.requests = append(h.requests, "ADD")
		n, _ := strconv.Atoi(strings.TrimPrefix(str("content"), "I_node"))
		if h.issues[n] == nil || str("project") != "PVT_board" {
			fail("NOT_FOUND")
			return
		}
		if b.cards[n] == nil {
			b.cards[n] = &fakeCard{}
		}
		answer(map[string]any{"addProjectV2ItemById": map[string]any{"item": map[string]string{"id": fmt.Sprintf("PVTI_%d", n)}}})
	case strings.Contains(in.Query, "issue(number:"):
		h.requests = append(h.requests, "ISSUE")
		fi := h.issues[num("n")]
		if fi == nil || !strings.EqualFold(str("owner"), owner) || str("repo") != "app" {
			fail("NOT_FOUND")
			return
		}
		m := h.gqlIssue(fi)
		// A card on somebody else's board must not count.
		other := map[string]any{"id": "PVTI_other", "isArchived": false, "status": map[string]string{"name": "Todo"},
			"worker":  map[string]string{"name": "linux"},
			"project": map[string]any{"number": b.number, "owner": map[string]string{"login": "stranger"}}}
		nodes := []any{other}
		if c := b.cards[fi.number]; c != nil {
			nodes = append(nodes, h.gqlCard(fi.number, c, c.status))
		}
		m["projectItems"] = map[string]any{"nodes": nodes}
		answer(map[string]any{"repository": map[string]any{"issue": m}})
	case strings.Contains(in.Query, "items(first:"):
		h.requests = append(h.requests, "CARDS")
		if !onBoard {
			fail("NOT_FOUND")
			return
		}
		if str("status") != "Status" || str("worker") != "Worker" {
			fail("UNPROCESSABLE")
			return
		}
		var nodes []any
		for _, c := range b.extra {
			m := h.gqlCard(0, c, c.status)
			switch c.kind {
			case "draft":
				m["content"] = map[string]any{"__typename": "DraftIssue"}
			case "pr":
				m["content"] = map[string]any{"__typename": "PullRequest"}
			case "hidden":
				m["content"] = nil
			default:
				other := h.gqlIssue(&fakeIssue{number: 900, title: "Elsewhere", author: owner, state: "open", nodeID: "I_else"})
				other["repository"] = map[string]string{"nameWithOwner": owner + "/other"}
				m["content"] = other
			}
			nodes = append(nodes, m)
		}
		var nums []int
		for n := range b.cards {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			c := b.cards[n]
			status := c.status
			if stale, ok := b.listStatus[n]; ok {
				status = stale
			}
			m := h.gqlCard(n, c, status)
			m["content"] = h.gqlIssue(h.issues[n])
			nodes = append(nodes, m)
		}
		start, _ := strconv.Atoi(str("after"))
		end := min(start+h.pageSize, len(nodes))
		info := map[string]any{"hasNextPage": end < len(nodes), "endCursor": strconv.Itoa(end)}
		answer(wrap(map[string]any{"items": map[string]any{"pageInfo": info, "nodes": nodes[start:end]}}))
	case strings.Contains(in.Query, "fields(first:"):
		h.requests = append(h.requests, "BOARD")
		if !onBoard {
			answer(wrap(nil))
			return
		}
		var opts, flows []any
		for _, col := range b.columns {
			opts = append(opts, map[string]string{"id": optionID(col), "name": col})
		}
		for _, name := range b.automation {
			flows = append(flows, map[string]any{"name": name, "enabled": true})
		}
		fields := []any{map[string]any{"__typename": "ProjectV2Field", "id": "F_title", "name": "Title"},
			map[string]any{"__typename": "ProjectV2SingleSelectField", "id": "F_status", "name": "Status", "options": opts}}
		if !b.noWorkers {
			var names []any
			for _, name := range b.workers {
				names = append(names, map[string]string{"id": optionID(name), "name": name})
			}
			fields = append(fields, map[string]any{"__typename": "ProjectV2SingleSelectField", "id": "F_worker", "name": "Worker", "options": names})
		}
		answer(wrap(map[string]any{"id": "PVT_board", "closed": b.closed, "fields": map[string]any{"nodes": fields},
			"workflows": map[string]any{"nodes": flows}}))
	default:
		w.WriteHeader(400)
	}
}

func boardCfg(apiURL string) *config.Config {
	cfg := testCfg(apiURL)
	cfg.Tracker.Provider["state_source"] = "project"
	cfg.Tracker.Provider["project"] = map[string]any{"owner": boardOrg, "number": 7}
	return cfg
}

func boardAdapter(t *testing.T, h *hub) *Adapter {
	t.Helper()
	a, err := New(boardCfg(h.srv.URL), Options{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var active = []string{"Todo", "Rework", "In Progress"}

func TestBoardColumnIsTheStateAndWorkerFieldRoutes(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux")
	h.card(2, "ian", "todo", "Mac")
	h.card(3, "stranger", "Todo", "linux")
	h.card(4, "ian", "Backlog", "linux")
	h.card(5, "ian", "In Progress", "")
	h.card(6, "ian", "Todo", "linux")
	h.board.cards[6].archived = true
	h.card(8, "ian", "", "linux")
	h.add(9, "ian", "ian", "status:todo", "run:linux") // not on the board at all
	h.board.extra = []*fakeCard{{status: "Todo", worker: "linux", kind: "draft"}, {status: "Todo", worker: "linux", kind: "pr"},
		{status: "Todo", worker: "linux", kind: "hidden"}, {status: "Todo", worker: "linux", kind: "elsewhere"}}
	// A label that looks like routing must not route a card.
	h.label(5, "ian", "run:linux")
	h.label(1, "ian", "P2")

	got, err := boardAdapter(t, h).FetchIssuesByStates(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	m := byIdent(got)
	if len(m) != 4 {
		t.Fatalf("got %d issues, want GH-1, GH-2, GH-3 and GH-5: %v", len(m), got)
	}
	one := m["GH-1"]
	if !one.Dispatchable || one.State != "Todo" || !one.HasLabels([]string{"run:linux"}) || one.ID != "I_node1" ||
		one.NativeRef["project_item_id"] != "PVTI_1" || one.Priority == nil || *one.Priority != 2 || one.AuthorID != "ian" {
		t.Errorf("GH-1 = %+v", one)
	}
	if two := m["GH-2"]; !two.Dispatchable || two.HasLabels([]string{"run:linux"}) || !two.HasLabels([]string{"run:mac"}) {
		t.Errorf("GH-2 belongs to the other worker: %+v", two)
	}
	if m["GH-3"].Dispatchable {
		t.Error("an issue written by a stranger is dispatchable")
	}
	if five := m["GH-5"]; five.HasLabels([]string{"run:linux"}) {
		t.Errorf("a run: label routed a card without a Worker value: %v", five.Labels)
	}
	if h.count("/issues?") != 0 || h.count("GET /repos/"+owner+"/app/issues ") != 0 {
		t.Error("the issue list was read in board mode")
	}
}

func TestBoardClosedIssueIsTerminal(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "In Progress", "linux").state = "closed"
	a := boardAdapter(t, h)
	if got, _ := a.FetchIssuesByStates(ctx, active); len(got) != 0 {
		t.Fatalf("a closed issue is active: %v", got)
	}
	got, err := a.FetchIssuesByStates(ctx, []string{"Done"})
	if err != nil || len(got) != 1 || got[0].State != "Done" || got[0].Dispatchable {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestBoardPagination(t *testing.T) {
	h := newBoardHub(t)
	h.pageSize = 2
	for n := 1; n <= 5; n++ {
		h.card(n, "ian", "Todo", "linux")
	}
	got, err := boardAdapter(t, h).FetchIssuesByStates(ctx, []string{"Todo"})
	if err != nil || len(got) != 5 || h.count("CARDS") != 3 {
		t.Fatalf("got %d issues in %d pages, %v", len(got), h.count("CARDS"), err)
	}
	if got, err := boardAdapter(t, h).FetchIssuesByStates(ctx, nil); err != nil || got != nil || h.count("CARDS") != 3 {
		t.Fatal("an empty state list made a request")
	}
}

func TestBoardFetchByIDsFollowsTheCard(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux")
	a := boardAdapter(t, h)
	first, err := a.FetchIssuesByStates(ctx, active)
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	ids := []string{first[0].ID, "I_unknown"}

	h.board.cards[1].status = "Blocked"
	got, err := a.FetchIssuesByIDs(ctx, ids)
	if err != nil || len(got) != 1 || got[0].State != "Blocked" {
		t.Fatalf("after a move: %v, %v", got, err)
	}
	h.board.cards[1].worker = "mac"
	if got, _ = a.FetchIssuesByIDs(ctx, ids); got[0].HasLabels([]string{"run:linux"}) {
		t.Fatal("a card given to another worker still routes here")
	}
	// Taken off the board: only a card on somebody else's board is left.
	delete(h.board.cards, 1)
	got, err = a.FetchIssuesByIDs(ctx, ids)
	if err != nil || len(got) != 1 || got[0].State != NoState || got[0].Dispatchable {
		t.Fatalf("off the board: %v, %v", got, err)
	}
	h.issues[1].nodeID = "I_replaced"
	if got, _ = a.FetchIssuesByIDs(ctx, ids); len(got) != 0 {
		t.Fatal("a different issue under the same number was returned")
	}
}

func TestBoardSetStateMovesTheCard(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux")
	a := boardAdapter(t, h)
	list, _ := a.FetchIssuesByStates(ctx, active)
	is := list[0]

	if err := a.SetState(ctx, is, "In Progress"); err != nil {
		t.Fatal(err)
	}
	if h.board.cards[1].status != "In Progress" || h.board.moves != 1 || len(h.issues[1].labels) != 0 {
		t.Fatalf("card = %+v, labels = %v", h.board.cards[1], h.issues[1].labels)
	}
	if err := a.SetState(ctx, is, "in progress"); err != nil || h.board.moves != 1 {
		t.Fatalf("a second write moved the card again: %v", err)
	}
	if err := a.SetState(ctx, is, "Todo"); tracker.CategoryOf(err) != tracker.WriteRejected {
		t.Fatalf("the approval column was written: %v", err)
	}

	// A column that is added later is found without a restart.
	h.board.columns = []string{"Todo", "In Progress"}
	a2 := boardAdapter(t, h)
	if err := a2.SetState(ctx, is, "Human Review"); tracker.CategoryOf(err) != tracker.WriteRejected || !strings.Contains(err.Error(), `no column "Human Review"`) {
		t.Fatalf("missing column: %v", err)
	}
	h.board.columns = append(h.board.columns, "Human Review")
	if err := a2.SetState(ctx, is, "Human Review"); err != nil || h.board.cards[1].status != "Human Review" {
		t.Fatalf("after the column was added: %v", err)
	}

	delete(h.board.cards, 1)
	if err := a.SetState(ctx, is, "Blocked"); tracker.CategoryOf(err) != tracker.WriteRejected || !strings.Contains(err.Error(), "no card") {
		t.Fatalf("without a card: %v", err)
	}
	wrong := is
	wrong.ID = "I_other"
	if err := a.SetState(ctx, wrong, "Blocked"); tracker.CategoryOf(err) != tracker.WriteRejected {
		t.Fatalf("another issue: %v", err)
	}
}

func TestBoardListLagAfterOwnWriteIsNotTrusted(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "In Progress", "linux")
	a := boardAdapter(t, h)
	list, _ := a.FetchIssuesByStates(ctx, active)
	if err := a.SetState(ctx, list[0], "Human Review"); err != nil {
		t.Fatal(err)
	}
	h.board.listStatus[1] = "In Progress" // the card list has not caught up
	got, err := a.FetchIssuesByStates(ctx, active)
	if err != nil || len(got) != 0 {
		t.Fatalf("a ticket that was handed off is active again: %v, %v", got, err)
	}
}

func TestBoardRequiredLabelStillNeedsAnAllowlistedLabeler(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux")
	h.card(2, "ian", "Todo", "linux")
	h.card(3, "ian", "Todo", "linux")
	h.label(1, "ian", "agent-ok")
	h.label(2, "stranger", "agent-ok")
	cfg := boardCfg(h.srv.URL)
	cfg.Tracker.RequiredLabels = []string{"agent-ok", "run:linux"}
	a, err := New(cfg, Options{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.FetchIssuesByStates(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	m := byIdent(got)
	if !m["GH-1"].Dispatchable || m["GH-2"].Dispatchable || m["GH-3"].Dispatchable {
		t.Fatalf("dispatchable: 1=%v 2=%v 3=%v", m["GH-1"].Dispatchable, m["GH-2"].Dispatchable, m["GH-3"].Dispatchable)
	}
}

func TestBoardWithoutLabelsReadsNoTimeline(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux")
	if _, err := boardAdapter(t, h).FetchIssuesByStates(ctx, active); err != nil {
		t.Fatal(err)
	}
	if h.count("timeline") != 0 {
		t.Fatal("the timeline was read although nothing depends on a label")
	}
}

func TestBoardBlockers(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux").blockers = []int{2}
	h.card(2, "ian", "Backlog", "linux")
	a := boardAdapter(t, h)
	got, err := a.FetchIssuesByStates(ctx, active)
	if err != nil || len(got) != 1 || got[0].Dispatchable || len(got[0].BlockedBy) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestBoardProblemsAreExplained(t *testing.T) {
	for name, c := range map[string]struct {
		edit     func(*hub, *config.Config)
		category string
		text     string
	}{
		"wrong number": {func(_ *hub, c *config.Config) {
			c.Tracker.Provider["project"] = map[string]any{"owner": boardOrg, "number": 8}
		}, tracker.Status, "was not found"},
		"not found error": {func(h *hub, _ *config.Config) { h.board.errors["fields(first:"] = "NOT_FOUND" }, tracker.Status, "Projects, read and write"},
		"closed":          {func(h *hub, _ *config.Config) { h.board.closed = true }, tracker.Status, "is closed"},
		"no status field": {func(_ *hub, c *config.Config) {
			c.Tracker.Provider["project"] = map[string]any{"owner": boardOrg, "number": 7, "status_field": "Stage"}
		}, tracker.Status, `no single-select field named "Stage"`},
		"forbidden":    {func(h *hub, _ *config.Config) { h.board.errors["items(first:"] = "FORBIDDEN" }, tracker.Status, "refused"},
		"rate limited": {func(h *hub, _ *config.Config) { h.board.errors["items(first:"] = "RATE_LIMITED" }, tracker.RateLimited, ""},
		"other":        {func(h *hub, _ *config.Config) { h.board.errors["items(first:"] = "INTERNAL" }, tracker.Response, "Could not  resolve"},
	} {
		h := newBoardHub(t)
		h.card(1, "ian", "Todo", "linux")
		cfg := boardCfg(h.srv.URL)
		c.edit(h, cfg)
		a, err := New(cfg, Options{Env: env})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = a.FetchIssuesByStates(ctx, active)
		if tracker.CategoryOf(err) != c.category || !strings.Contains(err.Error(), c.text) || strings.Contains(err.Error(), "\x07") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestBoardWriteRefusalIsAWriteError(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Todo", "linux")
	a := boardAdapter(t, h)
	list, _ := a.FetchIssuesByStates(ctx, active)
	h.board.errors["updateProjectV2ItemFieldValue"] = "FORBIDDEN"
	if err := a.SetState(ctx, list[0], "Blocked"); tracker.CategoryOf(err) != tracker.WriteRejected {
		t.Fatalf("err = %v", err)
	}
}

func TestBoardConfigValidation(t *testing.T) {
	h := newBoardHub(t)
	for name, project := range map[string]any{
		"missing":       nil,
		"not a mapping": "7",
		"no number":     map[string]any{"owner": boardOrg},
		"zero":          map[string]any{"number": 0},
		"text number":   map[string]any{"number": "7"},
		"fraction":      map[string]any{"number": 7.5},
		"bad owner":     map[string]any{"number": 7, "owner": "a/b"},
		"bot owner":     map[string]any{"number": 7, "owner": "app[bot]"},
		"bad field":     map[string]any{"number": 7, "status_field": "a\"b"},
		"same field":    map[string]any{"number": 7, "status_field": "X", "worker_field": "x"},
		"unknown key":   map[string]any{"number": 7, "token": "$T"},
		"wrong type":    map[string]any{"number": 7, "owner": 5},
	} {
		cfg := boardCfg(h.srv.URL)
		if project == nil {
			delete(cfg.Tracker.Provider, "project")
		} else {
			cfg.Tracker.Provider["project"] = project
		}
		if _, err := New(cfg, Options{Env: env}); tracker.CategoryOf(err) != tracker.InvalidConfig {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The owner defaults to the repository's, and YAML numbers come in several types.
	for _, n := range []any{7, int64(7), uint64(7), float64(7)} {
		cfg := boardCfg(h.srv.URL)
		cfg.Tracker.Provider["project"] = map[string]any{"number": n}
		a, err := New(cfg, Options{Env: env})
		if err != nil || a.board.owner != owner || a.board.number != 7 {
			t.Errorf("%T: %v", n, err)
		}
	}
	cfg := testCfg(h.srv.URL)
	cfg.Tracker.Provider["project"] = map[string]any{"number": 7}
	if _, err := New(cfg, Options{Env: env}); tracker.CategoryOf(err) != tracker.InvalidConfig {
		t.Errorf("a project with state_source labels was accepted: %v", err)
	}
}
