package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestWorkflowFiles(t *testing.T) {
	status, body := http.StatusOK, `[{"name":"ci.yml","type":"file"},{"name":"README.md","type":"file"},`+
		`{"name":"bad name;.yml","type":"file"},{"name":"release.yaml","type":"file"},{"name":"x","type":"dir"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/team/app/contents/.github/workflows" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()
	if found, known := workflowFiles(ctx, srv.URL, "team/app", "tok"); !known || !slices.Equal(found, []string{"ci.yml", "release.yaml"}) {
		t.Errorf("found %q known %v", found, known)
	}
	status, body = http.StatusNotFound, `{}`
	if found, known := workflowFiles(ctx, srv.URL, "team/app", "tok"); !known || len(found) != 0 {
		t.Errorf("no folder: found %q known %v", found, known)
	}
	status = http.StatusForbidden
	if _, known := workflowFiles(ctx, srv.URL, "team/app", "tok"); known {
		t.Error("a refusal counts as known")
	}
}
