package cli

import (
	"net/http"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marcioapm/lux/internal/proto"
)

// lux attach asks luxd for a restore with --restore only; without a
// terminal on stdin it sends no size.
func TestAttachRestoreQuery(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"attach", "run_1"}, ""},
		{[]string{"attach", "--restore", "run_1"}, "restore=true"},
	} {
		urls := make(chan string, 1)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			urls <- r.URL.Path + "?" + r.URL.RawQuery
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			_ = wsjson.Write(r.Context(), ws, proto.StreamData{Channel: "stdout", Data: []byte("screen")})
			_ = wsjson.Write(r.Context(), ws, proto.StreamData{Error: "the workload exited"})
		})
		out, err := runCLI(t, h, tc.args...)
		if err == nil || err.Error() != "the workload exited" {
			t.Fatalf("%v: error %v", tc.args, err)
		}
		if out != "screen" {
			t.Fatalf("%v: output %q", tc.args, out)
		}
		if got := <-urls; got != "/v1/runs/run_1/attach?"+tc.want {
			t.Fatalf("%v: dialled %q, want query %q", tc.args, got, tc.want)
		}
	}
}
