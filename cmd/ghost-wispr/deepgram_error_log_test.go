package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
)

// TestADeepgramLiveErrorLogsOnlyItsCode: Deepgram's own text, in an Error
// message it sends (whatever its fields say) or in the reason of a close, is
// its message, which can quote what it heard; the log keeps Deepgram's code,
// and a close's code.
func TestADeepgramLiveErrorLogsOnlyItsCode(t *testing.T) {
	const planted = "quokkaplantedroomword"
	logged := logtest.Capture(t)
	fake, host := startDeepgramEndpoint(t)
	dialDeepgramEndpoint(t, host, nil)
	conn := <-fake.conn
	for _, fields := range []map[string]string{
		{"type": "Error", "err_code": "DATA-0000", "err_msg": "Heard " + planted, "description": "Could not process " + planted, "variant": "x"},
		{"type": "Error", "err_code": "NET-0001", "err_msg": "socket", "description": "Could not process " + planted},
	} {
		message, _ := json.Marshal(fields)
		if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "Stopped at "+planted), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	var deepgramErrors []string
	for deadline := time.Now().Add(testWait); len(deepgramErrors) < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log holds %d Deepgram error records, want the two Error messages' and the close's: %+v", len(deepgramErrors), logged.Records())
		}
		deepgramErrors = deepgramErrors[:0]
		for _, record := range logged.Records() {
			if record.Message == "deepgram error" {
				fields, _ := json.Marshal(record.Attrs)
				deepgramErrors = append(deepgramErrors, string(fields))
			}
		}
	}
	for _, record := range logged.Records() {
		text, _ := json.Marshal(record)
		if strings.Contains(string(text), planted) {
			t.Errorf("a record holds Deepgram's text: %s", text)
		}
	}
	joined := strings.Join(deepgramErrors, "\n")
	if !strings.Contains(joined, `"code":"DATA-0000"`) || !strings.Contains(joined, `"code":"NET-0001"`) || !strings.Contains(joined, `"close_code":1011`) {
		t.Errorf("the Deepgram error records' fields %s do not name the codes DATA-0000 and NET-0001 and the close code 1011", joined)
	}
}
