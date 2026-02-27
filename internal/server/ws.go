package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func registerWSRoute(mux *http.ServeMux, hub *Hub) {
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws upgrade error: %v", err)
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
