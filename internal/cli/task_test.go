package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveTaskID_ResolvesShortHubTaskID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/tasks/01234567":
			w.WriteHeader(http.StatusNotFound)
		case "/admin/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{"hub_task_id": "01234567-89ab-cdef-0123-456789abcdef"}},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := &adminClient{baseURL: srv.URL, http: srv.Client()}
	got, err := resolveTaskID(c, "01234567")
	if err != nil {
		t.Fatalf("resolveTaskID: %v", err)
	}
	want := "01234567-89ab-cdef-0123-456789abcdef"
	if got != want {
		t.Fatalf("resolveTaskID = %q, want %q", got, want)
	}
}
