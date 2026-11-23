package trace

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// field is one decoded protobuf field: a number, or bytes that may hold a
// nested message.
type field struct {
	num   int
	n     uint64
	bytes []byte
}

func fields(t *testing.T, b []byte) []field {
	t.Helper()
	var out []field
	for len(b) > 0 {
		tag, k := binary.Uvarint(b)
		if k <= 0 {
			t.Fatal("bad tag")
		}
		b = b[k:]
		f := field{num: int(tag >> 3)}
		switch tag & 7 {
		case 0:
			v, k := binary.Uvarint(b)
			f.n, b = v, b[k:]
		case 1:
			f.n, b = binary.LittleEndian.Uint64(b), b[8:]
		case 2:
			l, k := binary.Uvarint(b)
			b = b[k:]
			f.bytes, b = b[:l], b[l:]
		default:
			t.Fatalf("wire type %d", tag&7)
		}
		out = append(out, f)
	}
	return out
}

func sub(t *testing.T, b []byte, path ...int) [][]byte {
	t.Helper()
	cur := [][]byte{b}
	for _, n := range path {
		var next [][]byte
		for _, m := range cur {
			for _, f := range fields(t, m) {
				if f.num == n {
					next = append(next, f.bytes)
				}
			}
		}
		cur = next
	}
	return cur
}

type decoded struct {
	name   string
	parent []byte
	trace  []byte
	attrs  map[string]string
	ints   map[string]uint64
	status uint64
}

func decode(t *testing.T, body []byte) (resource map[string]string, spans []decoded) {
	t.Helper()
	attrs := func(list [][]byte) (map[string]string, map[string]uint64) {
		s, n := map[string]string{}, map[string]uint64{}
		for _, a := range list {
			var key string
			for _, f := range fields(t, a) {
				switch f.num {
				case 1:
					key = string(f.bytes)
				case 2:
					for _, v := range fields(t, f.bytes) {
						if v.num == 1 {
							s[key] = string(v.bytes)
						} else if v.num == 3 {
							n[key] = v.n
						}
					}
				}
			}
		}
		return s, n
	}
	resource, _ = attrs(sub(t, body, 1, 1, 1))
	for _, sp := range sub(t, body, 1, 2, 2) {
		d := decoded{}
		var kvs [][]byte
		for _, f := range fields(t, sp) {
			switch f.num {
			case 1:
				d.trace = f.bytes
			case 4:
				d.parent = f.bytes
			case 5:
				d.name = string(f.bytes)
			case 9:
				kvs = append(kvs, f.bytes)
			case 15:
				for _, s := range fields(t, f.bytes) {
					if s.num == 3 {
						d.status = s.n
					}
				}
			}
		}
		d.attrs, d.ints = attrs(kvs)
		spans = append(spans, d)
	}
	return resource, spans
}

type collector struct {
	mu     sync.Mutex
	bodies [][]byte
	got    chan struct{}
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	c := &collector{got: make(chan struct{}, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-protobuf" || r.URL.Path != "/v1/traces" {
			http.Error(w, "no", http.StatusUnsupportedMediaType)
			return
		}
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.mu.Unlock()
		c.got <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *collector) wait(t *testing.T) []byte {
	t.Helper()
	select {
	case <-c.got:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was sent")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[len(c.bodies)-1]
}

const stream = `{"type":"system","subtype":"init","session_id":"s-1","model":"claude-x"}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100},"content":[{"type":"thinking","thinking":"The door should open slower."}]}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"I'll change the door speed."}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"go test ./..."}},{"type":"tool_use","id":"tu2","name":"Read","input":{"file_path":"x"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"FAIL door_test"}],"is_error":true}]}}
not json
{"type":"result","subtype":"success","result":"Done: doors open slower.","total_cost_usd":0.25,"usage":{"input_tokens":20,"output_tokens":9}}`

func TestATurnBecomesATrace(t *testing.T) {
	c, srv := newCollector(t)
	e := New("v1.2.3", nil)
	tr := e.Turn(Endpoint{URL: srv.URL + "/v1/traces", Project: "mini"}, "GH-26", "Make the doors slower.", "")
	clock := time.Unix(1000, 0)
	tr.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for _, l := range strings.Split(stream, "\n") {
		tr.Line([]byte(l))
	}
	tr.End(nil)

	var spans []decoded
	var res map[string]string
	deadline := time.After(5 * time.Second)
	for {
		r, s := decode(t, c.wait(t))
		res = r
		spans = append(spans, s...)
		if len(spans) >= 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d spans", len(spans))
		default:
		}
	}
	if res["openinference.project.name"] != "mini" || res["service.name"] != "maestro" {
		t.Errorf("resource %v", res)
	}
	by := map[string]decoded{}
	for _, s := range spans {
		by[s.name] = s
	}
	root := by["turn GH-26"]
	if root.parent != nil || root.attrs["openinference.span.kind"] != "AGENT" || root.attrs["session.id"] != "GH-26" ||
		root.attrs["input.value"] != "Make the doors slower." || root.attrs["output.value"] != "Done: doors open slower." ||
		root.attrs["llm.model_name"] != "claude-x" || root.ints["llm.token_count.completion"] != 9 || root.status != 1 {
		t.Errorf("root %+v", root)
	}
	think := by["thinks"]
	if think.attrs["output.value"] != "The door should open slower." || think.ints["llm.token_count.prompt"] != 110 ||
		string(think.parent) == "" || string(think.trace) != string(root.trace) {
		t.Errorf("thinking %+v", think)
	}
	if says := by["says"]; says.attrs["output.value"] != "I'll change the door speed." || says.ints["llm.token_count.prompt"] != 0 {
		t.Errorf("the message's usage is counted twice, or the text is lost: %+v", says)
	}
	bash := by["Bash"]
	if bash.attrs["openinference.span.kind"] != "TOOL" || bash.attrs["input.value"] != `{"command":"go test ./..."}` ||
		bash.attrs["output.value"] != "FAIL door_test" || bash.status != 2 {
		t.Errorf("bash %+v", bash)
	}
	if read := by["Read"]; read.status != 2 {
		t.Errorf("a tool still open at the end is not marked: %+v", read)
	}
}

func TestAFailedTurnIsMarked(t *testing.T) {
	c, srv := newCollector(t)
	tr := New("", nil).Turn(Endpoint{URL: srv.URL + "/v1/traces", Project: "p"}, "GH-1", "x", "m")
	tr.End(errors.New("the agent was silent for too long"))
	_, spans := decode(t, c.wait(t))
	if len(spans) != 1 || spans[0].status != 2 {
		t.Fatalf("spans %+v", spans)
	}
	tr.End(nil) // a second End sends nothing
	tr.Line([]byte(`{"type":"result"}`))
}

func TestANilTurnDoesNothing(t *testing.T) {
	var tr *Turn
	tr.Line([]byte("{}"))
	tr.End(nil)
}

func TestClip(t *testing.T) {
	long := strings.Repeat("é", MaxValue)
	got := clip(long)
	if len(got) > MaxValue+10 || !strings.HasSuffix(got, "[cut]") || !strings.HasPrefix(got, "é") {
		t.Errorf("clip gave %d bytes", len(got))
	}
	if clip("ok") != "ok" {
		t.Error("short text changed")
	}
}

func TestACollectorThatIsDownDoesNotBlock(t *testing.T) {
	e := New("", nil)
	tr := e.Turn(Endpoint{URL: "http://127.0.0.1:1/v1/traces", Project: "p"}, "GH-1", "x", "")
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueLen*3; i++ {
			e.send(tr.ep, []span{tr.root})
		}
		tr.End(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sending blocked")
	}
}
