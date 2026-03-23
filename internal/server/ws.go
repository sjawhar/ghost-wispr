package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sjawhar/ghost-wispr/internal/logging"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func registerWSRoute(mux *http.ServeMux, hub *Hub, logger *slog.Logger) {
	moduleLogger := logging.WithModule(logger, "server")

	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logging.FromContext(r.Context(), moduleLogger).Warn("websocket upgrade failed", "operation", "ws_upgrade", "error", err)
			return
		}
		defer func() { _ = conn.Close() }()

		connectionEvent := ConnectionEvent{
			Event:     newEvent("connection", time.Now().UTC()),
			Connected: true,
		}
		payload, err := json.Marshal(connectionEvent)
		if err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, payload)
		}

		ch := hub.Subscribe()
		defer hub.Unsubscribe(ch)

		for msg := range ch {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	})
}
