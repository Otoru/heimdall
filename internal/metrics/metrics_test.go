package metrics

import "testing"

func TestHandlerFor(t *testing.T) {
	m := New()
	handler := HandlerFor(m)
	if handler == nil {
		t.Fatalf("expected handler")
	}
}
