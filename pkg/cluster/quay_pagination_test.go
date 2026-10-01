package cluster

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReleaseDiscoveryOriginalParallelPaging(t *testing.T) {
	resetReleaseTagCache(t)
	var followedRealCursor atomic.Bool
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		cursor := r.URL.Query().Get("last")
		if strings.HasSuffix(cursor, ".") {
			t.Errorf("fabricated punctuation cursor: %s", cursor)
		}
		body := `{"tags":[]}`
		switch cursor {
		case "rhoai-3.5":
			body = `{"tags":["rhoai-3.5","rhoai-3.5-19483fbe7e963888b951664254d0f9de91db7db4"]}`
		case "rhoai-3.5-19483fbe7e963888b951664254d0f9de91db7db4":
			followedRealCursor.Store(true)
			body = `{"tags":["rhoai-3.7-ea","rhoai-3.7","rhoai-8.12-ea","unrelated"]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	tags, err := fetchAndParseTags(context.Background(), client, "test")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tag := range tags {
		got = append(got, tag.raw)
	}
	want := []string{"rhoai-3.5", "rhoai-3.7-ea", "rhoai-3.7", "rhoai-8.12-ea"}
	if !followedRealCursor.Load() || !reflect.DeepEqual(got, want) {
		t.Fatalf("followed actual cursor=%v, tags=%v", followedRealCursor.Load(), got)
	}
}
