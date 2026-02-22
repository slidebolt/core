package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	sdk "plugin-sdk"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// hub broadcasts messages to all connected WebSocket clients.
type hub struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[chan []byte]struct{})}
}

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *hub) broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default: // slow client — drop rather than block
		}
	}
}

func (h *hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Server holds shared state for all HTTP/WS handlers.
type Server struct {
	nc  *nats.Conn
	hub *hub

	regMu          sync.RWMutex
	entityToDevice map[string]string          // entity UUID → device UUID
	registry       map[string]json.RawMessage // device UUID → raw JSON (persistent cache)

	entityStateMu   sync.RWMutex
	entityLiveState map[string]map[string]any // entity UUID → last published live state
}

func NewServer(nc *nats.Conn) *Server {
	s := &Server{
		nc:              nc,
		hub:             newHub(),
		entityToDevice:  make(map[string]string),
		registry:        make(map[string]json.RawMessage),
		entityLiveState: make(map[string]map[string]any),
	}

	// Subscribe to entity state updates; cache and fan-out to connected WS clients.
	nc.Subscribe("entity.*.state", func(msg *nats.Msg) {
		// Subject format: "entity.<entityId>.state"
		parts := strings.Split(msg.Subject, ".")
		if len(parts) != 3 {
			return
		}
		entityID := parts[1]

		s.regMu.RLock()
		deviceID, ok := s.entityToDevice[entityID]
		s.regMu.RUnlock()

		if !ok {
			log.Printf("[state] unknown entity %s — rebuilding map", entityID)
			go s.rebuildAndDiff()
			return
		}

		payload := extractStatePayload(msg.Data)

		// Cache the live state so new WS clients get it in their snapshot.
		if payloadMap, ok := payload.(map[string]any); ok {
			s.entityStateMu.Lock()
			if existing, ok := s.entityLiveState[entityID]; ok {
				for k, v := range payloadMap {
					existing[k] = v
				}
			} else {
				clone := make(map[string]any, len(payloadMap))
				for k, v := range payloadMap {
					clone[k] = v
				}
				s.entityLiveState[entityID] = clone
			}
			s.entityStateMu.Unlock()
		}

		out, _ := json.Marshal(map[string]any{
			"type":         "state",
			"id":           deviceID,
			"entity_state": map[string]any{entityID: payload},
		})
		log.Printf("[state] entity=%s device=%s → %d client(s)", entityID, deviceID, s.hub.clientCount())
		s.hub.broadcast(out)
	})

	// Subscribe to device registration events.
	nc.Subscribe("registry.device.register", func(msg *nats.Msg) {
		log.Printf("[registry] register event received")
		go s.rebuildAndDiff()
	})

	// Subscribe to device unregistration events.
	nc.Subscribe("registry.device.unregister", func(msg *nats.Msg) {
		log.Printf("[registry] unregister event received")
		go s.rebuildAndDiff()
	})

	return s
}

// extractStatePayload normalizes bus messages so websocket clients receive only
// the effective entity state object, not the SDK envelope.
func extractStatePayload(raw []byte) any {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	obj, ok := decoded.(map[string]any)
	if !ok {
		return decoded
	}
	if inner, ok := obj["payload"]; ok {
		if payloadObj, ok := inner.(map[string]any); ok {
			return payloadObj
		}
	}
	return obj
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/ws", s.handleWS)
	mux.HandleFunc("/api/bundles", s.handleBundles)
	mux.HandleFunc("/api/registry", s.handleRegistry)
	mux.HandleFunc("/api/publish", s.handlePublish)
	mux.HandleFunc("/api/mcp/catalog", s.handleMCPCatalog)
	mux.HandleFunc("/api/mcp/call", s.handleMCPCall)
	log.Printf("[api] routes registered: /api/ws /api/bundles /api/registry /api/publish /api/mcp/catalog /api/mcp/call")
}

// discoverBundles scatter-gathers bundle.discovery and returns raw JSON for each bundle.
func (s *Server) discoverBundles() []json.RawMessage {
	inbox := nats.NewInbox()
	ch := make(chan json.RawMessage, 100)

	sub, _ := s.nc.Subscribe(inbox, func(m *nats.Msg) {
		if len(m.Data) == 0 || !json.Valid(m.Data) {
			return // NATS "no responders" status — ignore
		}
		ch <- json.RawMessage(m.Data)
	})
	defer sub.Unsubscribe()

	s.nc.PublishRequest("bundle.discovery", inbox, nil)

	var list []json.RawMessage
	timeout := time.After(400 * time.Millisecond)
	for {
		select {
		case b := <-ch:
			list = append(list, b)
		case <-timeout:
			log.Printf("[discovery] found %d bundle(s)", len(list))
			return list
		}
	}
}

// fetchAllDevices queries every bundle for its devices, updates the persistent
// registry cache and entity→device mapping, and returns the new registry.
func (s *Server) fetchAllDevices() map[string]json.RawMessage {
	bundles := s.discoverBundles()
	fresh := make(map[string]json.RawMessage)
	entityToDevice := make(map[string]string)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)

	for _, bRaw := range bundles {
		var b struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(bRaw, &b); err != nil || b.ID == "" {
			continue
		}

		wg.Add(1)
		go func(bundleID string) {
			defer wg.Done()

			msg, err := s.nc.Request(
				fmt.Sprintf("bundle.%s.get_devices", bundleID),
				nil,
				500*time.Millisecond,
			)
			if err != nil {
				log.Printf("[registry] bundle=%s get_devices failed: %v", bundleID, err)
				return
			}

			var devs []json.RawMessage
			if err := json.Unmarshal(msg.Data, &devs); err != nil {
				log.Printf("[registry] bundle=%s bad JSON: %v", bundleID, err)
				return
			}

			mu.Lock()
			defer mu.Unlock()
			for _, devRaw := range devs {
				var dev struct {
					UUID     string                     `json:"uuid"`
					Entities map[string]json.RawMessage `json:"entities"`
				}
				if err := json.Unmarshal(devRaw, &dev); err != nil || dev.UUID == "" {
					continue
				}
				fresh[dev.UUID] = devRaw
				for entityID := range dev.Entities {
					entityToDevice[entityID] = dev.UUID
				}
			}
			log.Printf("[registry] bundle=%s devices=%d", bundleID, len(devs))
		}(b.ID)
	}

	wg.Wait()

	s.regMu.Lock()
	s.entityToDevice = entityToDevice
	s.registry = fresh
	s.regMu.Unlock()

	log.Printf("[registry] total devices=%d entities=%d", len(fresh), len(entityToDevice))
	return fresh
}

// rebuildAndDiff fetches the latest device list and broadcasts register/unregister
// events to WS clients for anything that changed since last fetch.
func (s *Server) rebuildAndDiff() {
	log.Printf("[registry] rebuilding...")

	s.regMu.RLock()
	prev := make(map[string]struct{}, len(s.registry))
	for id := range s.registry {
		prev[id] = struct{}{}
	}
	s.regMu.RUnlock()

	fresh := s.fetchAllDevices()

	// Broadcast register events for newly discovered devices.
	for id, devRaw := range fresh {
		if _, existed := prev[id]; !existed {
			var dev struct {
				Entities map[string]json.RawMessage `json:"entities"`
			}
			json.Unmarshal(devRaw, &dev)

			// Collect cached live state for this device's entities.
			s.entityStateMu.RLock()
			entityStates := make(map[string]map[string]any)
			for entityID := range dev.Entities {
				if st, ok := s.entityLiveState[entityID]; ok {
					entityStates[entityID] = st
				}
			}
			s.entityStateMu.RUnlock()

			var devAny any
			json.Unmarshal(devRaw, &devAny)
			out, _ := json.Marshal(map[string]any{
				"type":          "register",
				"id":            id,
				"instance":      devAny,
				"entity_states": entityStates,
			})
			log.Printf("[registry] new device=%s → broadcasting register", id)
			s.hub.broadcast(out)
		}
	}

	// Broadcast unregister events for devices that vanished.
	for id := range prev {
		if _, stillExists := fresh[id]; !stillExists {
			out, _ := json.Marshal(map[string]any{
				"type": "unregister",
				"id":   id,
			})
			log.Printf("[registry] removed device=%s → broadcasting unregister", id)
			s.hub.broadcast(out)
		}
	}
}

// handleBundles serves GET /api/bundles
func (s *Server) handleBundles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bundles := s.discoverBundles()
	if bundles == nil {
		bundles = []json.RawMessage{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bundles)
}

// handleRegistry serves GET /api/registry
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	registry := s.fetchAllDevices()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(registry)
}

// handlePublish serves POST /api/publish — publishes a raw event onto the NATS bus.
// Body: {"topic": "device.<uuid>.command", "data": {...}}
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Topic == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	log.Printf("[publish] topic=%s data=%s", req.Topic, req.Data)
	if err := s.nc.Publish(req.Topic, req.Data); err != nil {
		log.Printf("[publish] failed: %v", err)
		http.Error(w, "publish failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWS serves GET /api/ws — upgrades to WebSocket, sends a snapshot immediately,
// then pushes incremental events. No client→server messages are processed.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade failed: %v", err)
		return
	}
	log.Printf("[ws] client connected: %s (total: %d)", r.RemoteAddr, s.hub.clientCount()+1)

	// Use cached registry if available, otherwise fetch fresh.
	s.regMu.RLock()
	cachedReg := s.registry
	s.regMu.RUnlock()

	var registry map[string]json.RawMessage
	if len(cachedReg) > 0 {
		registry = cachedReg
	} else {
		registry = s.fetchAllDevices()
	}

	bundles := s.discoverBundles()
	if bundles == nil {
		bundles = []json.RawMessage{}
	}

	s.entityStateMu.RLock()
	entityStates := make(map[string]map[string]any, len(s.entityLiveState))
	for k, v := range s.entityLiveState {
		entityStates[k] = v
	}
	s.entityStateMu.RUnlock()

	snapshot, _ := json.Marshal(map[string]any{
		"type":          "snapshot",
		"registry":      registry,
		"bundles":       bundles,
		"entity_states": entityStates,
	})
	log.Printf("[ws] sending snapshot: devices=%d bundles=%d", len(registry), len(bundles))

	var writeMu sync.Mutex
	writeMu.Lock()
	conn.WriteMessage(websocket.TextMessage, snapshot)
	writeMu.Unlock()

	// Subscribe this client to the broadcast hub.
	ch := s.hub.subscribe()

	// Fan-out goroutine: forwards hub messages and sends pings.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					conn.Close()
					return
				}
				writeMu.Lock()
				err := conn.WriteMessage(websocket.TextMessage, msg)
				writeMu.Unlock()
				if err != nil {
					conn.Close()
					return
				}
			case <-ticker.C:
				writeMu.Lock()
				err := conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	// Read loop: discard incoming messages, handle pongs, detect disconnect.
	conn.SetReadLimit(512)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	s.hub.unsubscribe(ch)
	log.Printf("[ws] client disconnected: %s (total: %d)", r.RemoteAddr, s.hub.clientCount())
}

func (s *Server) handleMCPCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := []map[string]any{
		{
			"bundle_id": "core",
			"mcp": map[string]any{
				"tools": []map[string]any{
					{"name": "list_devices", "description": "List all discovered devices in the registry"},
					{"name": "send_command", "description": "Send a command to a device by UUID"},
					{"name": "delete_device", "description": "Delete a device by UUID (purges entities/state)"},
					{"name": "set_entity_script", "description": "Set script for an entity UUID and hot-reload it"},
					{"name": "update_device_raw", "description": "Update raw data for a device by UUID"},
					{"name": "create_entity", "description": "Create a new entity on an existing device"},
					{"name": "configure_plugin", "description": "Update persistent config for a plugin bundle"},
					{"name": "publish", "description": "Publish a raw payload to a bus topic"},
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleMCPCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BundleID string                 `json:"bundle_id"`
		Tool     string                 `json:"tool"`
		Args     map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BundleID != "" && req.BundleID != "core" {
		http.Error(w, "unsupported bundle_id on core mcp endpoint", http.StatusBadRequest)
		return
	}
	if req.Args == nil {
		req.Args = map[string]interface{}{}
	}

	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	switch req.Tool {
	case "list_devices":
		registry := s.fetchAllDevices()
		writeJSON(registry)
		return
	case "send_command":
		uuid, _ := req.Args["uuid"].(string)
		cmd, _ := req.Args["command"].(string)
		if uuid == "" || cmd == "" {
			http.Error(w, "uuid and command are required", http.StatusBadRequest)
			return
		}
		subject := fmt.Sprintf("device.%s.command", uuid)
		data, _ := json.Marshal(sdk.Message{
			Subject: subject,
			Payload: map[string]interface{}{"command": cmd},
		})
		if err := s.nc.Publish(subject, data); err != nil {
			http.Error(w, "publish failed", http.StatusInternalServerError)
			return
		}
		writeJSON(map[string]any{"ok": true})
		return
	case "delete_device":
		uuid, _ := req.Args["uuid"].(string)
		if uuid == "" {
			http.Error(w, "uuid is required", http.StatusBadRequest)
			return
		}

		registry := s.fetchAllDevices()
		devRaw, ok := registry[uuid]
		if !ok {
			http.Error(w, "device not found", http.StatusNotFound)
			return
		}
		var dev struct {
			Bundle string `json:"bundle"`
		}
		if err := json.Unmarshal(devRaw, &dev); err != nil || dev.Bundle == "" {
			http.Error(w, "unable to resolve device bundle", http.StatusInternalServerError)
			return
		}

		payload, _ := json.Marshal(map[string]any{"uuid": uuid})
		respMsg, err := s.nc.Request(fmt.Sprintf("bundle.%s.delete_device", dev.Bundle), payload, 2*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("delete request failed: %v", err), http.StatusInternalServerError)
			return
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respMsg.Data, &resp); err == nil && !resp.OK {
			http.Error(w, fmt.Sprintf("delete failed: %s", resp.Error), http.StatusInternalServerError)
			return
		}

		_ = s.fetchAllDevices()
		writeJSON(map[string]any{"ok": true, "uuid": uuid, "bundle_id": dev.Bundle})
		return
	case "set_entity_script":
		entityUUID, _ := req.Args["entity_uuid"].(string)
		rawScript, hasScript := req.Args["script"]
		script, scriptIsString := rawScript.(string)
		if entityUUID == "" {
			http.Error(w, "entity_uuid is required", http.StatusBadRequest)
			return
		}
		if !hasScript || !scriptIsString {
			http.Error(w, "script (string) is required", http.StatusBadRequest)
			return
		}

		registry := s.fetchAllDevices()
		var owner struct {
			Bundle string
			Device string
		}
		for deviceUUID, devRaw := range registry {
			var dev struct {
				UUID     string                     `json:"uuid"`
				Bundle   string                     `json:"bundle"`
				Entities map[string]json.RawMessage `json:"entities"`
			}
			if err := json.Unmarshal(devRaw, &dev); err != nil {
				continue
			}
			if _, ok := dev.Entities[entityUUID]; ok {
				owner.Bundle = dev.Bundle
				owner.Device = dev.UUID
				if owner.Device == "" {
					owner.Device = deviceUUID
				}
				break
			}
		}
		if owner.Device == "" {
			http.Error(w, "entity not found", http.StatusNotFound)
			return
		}
		if owner.Bundle == "" {
			http.Error(w, "unable to resolve entity bundle", http.StatusInternalServerError)
			return
		}

		payload, _ := json.Marshal(map[string]any{
			"entity_uuid": entityUUID,
			"script":      script,
		})
		respMsg, err := s.nc.Request(fmt.Sprintf("bundle.%s.set_entity_script", owner.Bundle), payload, 2*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("set_entity_script request failed: %v", err), http.StatusInternalServerError)
			return
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respMsg.Data, &resp); err == nil && !resp.OK {
			http.Error(w, fmt.Sprintf("set_entity_script failed: %s", resp.Error), http.StatusInternalServerError)
			return
		}

		writeJSON(map[string]any{
			"ok":          true,
			"bundle_id":   owner.Bundle,
			"device_uuid": owner.Device,
			"entity_uuid": entityUUID,
		})
		return
	case "update_device_raw":
		deviceUUID, _ := req.Args["device_uuid"].(string)
		raw, _ := req.Args["raw"].(map[string]interface{})
		if deviceUUID == "" {
			http.Error(w, "device_uuid is required", http.StatusBadRequest)
			return
		}
		if raw == nil {
			http.Error(w, "raw object is required", http.StatusBadRequest)
			return
		}

		registry := s.fetchAllDevices()
		devRaw, ok := registry[deviceUUID]
		if !ok {
			http.Error(w, "device not found", http.StatusNotFound)
			return
		}
		var devBundle struct {
			Bundle string `json:"bundle"`
		}
		if err := json.Unmarshal(devRaw, &devBundle); err != nil || devBundle.Bundle == "" {
			http.Error(w, "unable to resolve device bundle", http.StatusInternalServerError)
			return
		}

		payload, _ := json.Marshal(map[string]any{"device_uuid": deviceUUID, "raw": raw})
		respMsg, err := s.nc.Request(fmt.Sprintf("bundle.%s.update_device_raw", devBundle.Bundle), payload, 2*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("update_device_raw request failed: %v", err), http.StatusInternalServerError)
			return
		}
		var rawResp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respMsg.Data, &rawResp); err == nil && !rawResp.OK {
			http.Error(w, fmt.Sprintf("update_device_raw failed: %s", rawResp.Error), http.StatusInternalServerError)
			return
		}
		writeJSON(map[string]any{"ok": true, "device_uuid": deviceUUID})
		return
	case "create_entity":
		deviceUUID, _ := req.Args["device_uuid"].(string)
		entityType, _ := req.Args["type"].(string)
		name, _ := req.Args["name"].(string)
		sourceID, _ := req.Args["source_id"].(string)
		if deviceUUID == "" || entityType == "" {
			http.Error(w, "device_uuid and type are required", http.StatusBadRequest)
			return
		}

		registry := s.fetchAllDevices()
		devRaw, ok := registry[deviceUUID]
		if !ok {
			http.Error(w, "device not found", http.StatusNotFound)
			return
		}
		var devBundle struct {
			Bundle string `json:"bundle"`
		}
		if err := json.Unmarshal(devRaw, &devBundle); err != nil || devBundle.Bundle == "" {
			http.Error(w, "unable to resolve device bundle", http.StatusInternalServerError)
			return
		}

		payload, _ := json.Marshal(map[string]any{
			"device_uuid": deviceUUID,
			"type":        entityType,
			"name":        name,
			"source_id":   sourceID,
		})
		respMsg, err := s.nc.Request(fmt.Sprintf("bundle.%s.create_entity", devBundle.Bundle), payload, 2*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("create_entity request failed: %v", err), http.StatusInternalServerError)
			return
		}
		var entResp struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			EntityUUID string `json:"entity_uuid"`
		}
		if err := json.Unmarshal(respMsg.Data, &entResp); err != nil || !entResp.OK {
			http.Error(w, fmt.Sprintf("create_entity failed: %s", entResp.Error), http.StatusInternalServerError)
			return
		}
		writeJSON(map[string]any{"ok": true, "entity_uuid": entResp.EntityUUID, "device_uuid": deviceUUID})
		return
	case "configure_plugin":
		bundleID, _ := req.Args["bundle_id"].(string)
		config, _ := req.Args["config"].(map[string]interface{})
		if bundleID == "" {
			http.Error(w, "bundle_id is required", http.StatusBadRequest)
			return
		}
		if config == nil {
			http.Error(w, "config object is required", http.StatusBadRequest)
			return
		}

		payload, _ := json.Marshal(config)
		respMsg, err := s.nc.Request(fmt.Sprintf("bundle.%s.configure", bundleID), payload, 2*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("configure request failed: %v", err), http.StatusInternalServerError)
			return
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respMsg.Data, &resp); err == nil && !resp.OK {
			http.Error(w, fmt.Sprintf("configure failed: %s", resp.Error), http.StatusInternalServerError)
			return
		}

		writeJSON(map[string]any{"ok": true})
		return
	case "publish":
		topic, _ := req.Args["topic"].(string)
		rawData, hasData := req.Args["data"]
		if topic == "" || !hasData {
			http.Error(w, "topic and data are required", http.StatusBadRequest)
			return
		}
		data, err := json.Marshal(rawData)
		if err != nil {
			http.Error(w, "invalid data payload", http.StatusBadRequest)
			return
		}
		if err := s.nc.Publish(topic, data); err != nil {
			http.Error(w, "publish failed", http.StatusInternalServerError)
			return
		}
		writeJSON(map[string]any{"ok": true})
		return
	default:
		http.Error(w, "unknown tool", http.StatusBadRequest)
		return
	}
}
