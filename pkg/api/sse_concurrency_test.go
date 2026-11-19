package api

import (
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSSEConcurrentWrites(t *testing.T) {
	w, err := NewSSEWriter(httptest.NewRecorder())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for range 100 {
			w.SendHeartbeat()
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for range 100 {
			w.SendStep(UpdateStep{Step: "probe", Status: "running"})
		}
	}()
	close(start)
	workers.Wait()
}
