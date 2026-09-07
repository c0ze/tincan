package host

import (
	"context"
	"testing"
	"time"
)

func TestListenerNamesCannotCollideWithLifecycleLocks(t *testing.T) {
	room, _, _, stopped, _ := startServe(t, echoPreset(), "fake")
	ctx, cancel := context.WithCancel(context.Background())
	other := make(chan error, 1)
	go func() {
		other <- Serve(ctx, ServeOptions{Room: room, Name: "agent.launch", Label: "fake", Preset: echoPreset()})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-other:
			if err != nil {
				t.Errorf("second host: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("second host did not stop")
		}
	})
	if !waitFor(func() bool {
		st, ok, _ := ReadState(room, "agent.launch")
		return ok && st.Alive()
	}, 5*time.Second) {
		t.Fatal("second host did not become ready")
	}
	if err := Down(context.Background(), room, "agent", 2*time.Second); err != nil {
		t.Fatalf("other listener's name blocked shutdown: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("original host did not stop")
	}
	st, ok, err := ReadState(room, "agent.launch")
	if err != nil || !ok || !st.Alive() {
		t.Fatalf("shutdown affected other listener: %+v %v", st, err)
	}
}
