package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return res
}

func startTestNATS(t *testing.T) (*natsserver.Server, *nats.Conn) {
	t.Helper()

	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not become ready")
	}

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect nats: %v", err)
	}

	return ns, nc
}

func startTestAPI(t *testing.T, nc *nats.Conn) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	s := NewServer(nc)
	s.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

func TestHandlePublish_ForwardsRawEventToNATS(t *testing.T) {
	ns, nc := startTestNATS(t)
	defer ns.Shutdown()
	defer nc.Close()

	ts := startTestAPI(t, nc)
	defer ts.Close()

	sub, err := nc.SubscribeSync("entity.test.command")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	body := map[string]any{
		"topic": "entity.test.command",
		"data": map[string]any{
			"subject": "entity.test.command",
			"payload": map[string]any{"command": "TurnOn"},
		},
	}
	rawBody, _ := json.Marshal(body)
	res, err := http.Post(ts.URL+"/api/publish", "application/json", bytes.NewReader(rawBody))
	if err != nil {
		t.Fatalf("post publish: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status: got=%d want=%d", res.StatusCode, http.StatusNoContent)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected nats message: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("invalid nats payload: %v", err)
	}
	if got["subject"] != "entity.test.command" {
		t.Fatalf("unexpected subject in forwarded payload: %v", got["subject"])
	}
	payload, _ := got["payload"].(map[string]any)
	if payload["command"] != "TurnOn" {
		t.Fatalf("unexpected command in forwarded payload: %v", payload["command"])
	}
}

func TestWebSocket_StateBroadcast_UnwrapsSDKPayload(t *testing.T) {
	ns, nc := startTestNATS(t)
	defer ns.Shutdown()
	defer nc.Close()

	_, err := nc.Subscribe("bundle.discovery", func(msg *nats.Msg) {
		_ = nc.Publish(msg.Reply, []byte(`{"id":"test-bundle","name":"Test Bundle","status":"active"}`))
	})
	if err != nil {
		t.Fatalf("subscribe bundle.discovery: %v", err)
	}
	_, err = nc.Subscribe("bundle.test-bundle.get_devices", func(msg *nats.Msg) {
		_ = nc.Publish(msg.Reply, []byte(`[
			{
				"uuid":"dev-1",
				"bundle":"test-bundle",
				"metadata":{"id":"dev-1","name":"Device One"},
				"state":{"enabled":true,"status":"active"},
				"entities":{
					"ent-1":{
						"id":"ent-1",
						"metadata":{"type":"LIGHT","capabilities":["brightness"]}
					}
				}
			}
		]`))
	})
	if err != nil {
		t.Fatalf("subscribe bundle.get_devices: %v", err)
	}
	nc.Flush()

	ts := startTestAPI(t, nc)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, firstMsg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(firstMsg, &snapshot); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if snapshot["type"] != "snapshot" {
		t.Fatalf("expected snapshot first message, got: %v", snapshot["type"])
	}

	stateEnvelope := map[string]any{
		"source":    "test-bundle",
		"subject":   "entity.ent-1.state",
		"payload":   map[string]any{"state": "on", "brightness": float64(42)},
		"timestamp": time.Now().UnixNano(),
	}
	raw, _ := json.Marshal(stateEnvelope)
	if err := nc.Publish("entity.ent-1.state", raw); err != nil {
		t.Fatalf("publish state: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, stateMsg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read state message: %v", err)
	}

	var event map[string]any
	if err := json.Unmarshal(stateMsg, &event); err != nil {
		t.Fatalf("state json: %v", err)
	}
	if event["type"] != "state" {
		t.Fatalf("expected state event, got: %v", event["type"])
	}
	if event["id"] != "dev-1" {
		t.Fatalf("expected device id dev-1, got: %v", event["id"])
	}

	entityState, ok := event["entity_state"].(map[string]any)
	if !ok {
		t.Fatalf("entity_state missing or invalid: %T", event["entity_state"])
	}
	ent1, ok := entityState["ent-1"].(map[string]any)
	if !ok {
		t.Fatalf("ent-1 state missing or invalid: %T", entityState["ent-1"])
	}
	if ent1["state"] != "on" {
		t.Fatalf("expected unwrapped state=on, got: %v", ent1["state"])
	}
	if fmt.Sprintf("%.0f", ent1["brightness"]) != "42" {
		t.Fatalf("expected brightness=42, got: %v", ent1["brightness"])
	}
}

func TestExtractStatePayload(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "sdk envelope payload",
			raw:  `{"source":"x","subject":"entity.e.state","payload":{"state":"on"}}`,
			want: `{"state":"on"}`,
		},
		{
			name: "plain object passthrough",
			raw:  `{"state":"off","brightness":10}`,
			want: `{"brightness":10,"state":"off"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractStatePayload([]byte(tc.raw))
			gotRaw, _ := json.Marshal(got)

			var gotObj map[string]any
			var wantObj map[string]any
			_ = json.Unmarshal(gotRaw, &gotObj)
			_ = json.Unmarshal([]byte(tc.want), &wantObj)

			if len(gotObj) != len(wantObj) {
				t.Fatalf("payload shape mismatch: got=%s want=%s", string(gotRaw), tc.want)
			}
			for k, v := range wantObj {
				if gotObj[k] != v {
					t.Fatalf("payload mismatch at %s: got=%v want=%v", k, gotObj[k], v)
				}
			}
		})
	}
}

func TestMCPCall_SendCommand_EntityUUID_PublishesEntitySubjectAndFlattensPayload(t *testing.T) {
	ns, nc := startTestNATS(t)
	defer ns.Shutdown()
	defer nc.Close()

	mux := http.NewServeMux()
	s := NewServer(nc)
	s.regMu.Lock()
	s.entityToDevice["ent-1"] = "dev-1"
	s.regMu.Unlock()
	s.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	sub, err := nc.SubscribeSync("entity.ent-1.command")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	req := map[string]any{
		"bundle_id": "core",
		"tool":      "send_command",
		"args": map[string]any{
			"uuid":      "ent-1",
			"command":   "SetBrightness",
			"payload":   map[string]any{"level": 11},
			"entity_id": "ent-1",
		},
	}
	res := postJSON(t, ts.URL+"/api/mcp/call", req)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: got=%d want=%d", res.StatusCode, http.StatusOK)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected command message: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("invalid nats payload: %v", err)
	}
	if got["subject"] != "entity.ent-1.command" {
		t.Fatalf("unexpected subject: %v", got["subject"])
	}
	payload, _ := got["payload"].(map[string]any)
	if payload["command"] != "SetBrightness" {
		t.Fatalf("unexpected command: %v", payload["command"])
	}
	if fmt.Sprintf("%.0f", payload["level"]) != "11" {
		t.Fatalf("expected flattened level=11, got payload=%v", payload)
	}
	if _, nested := payload["payload"]; nested {
		t.Fatalf("payload should be flattened, got nested payload field: %v", payload["payload"])
	}
}

func TestMCPCall_SendCommand_DeviceUUID_PublishesDeviceSubject(t *testing.T) {
	ns, nc := startTestNATS(t)
	defer ns.Shutdown()
	defer nc.Close()

	mux := http.NewServeMux()
	s := NewServer(nc)
	s.regMu.Lock()
	s.registry["dev-1"] = json.RawMessage(`{"uuid":"dev-1"}`)
	s.regMu.Unlock()
	s.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	sub, err := nc.SubscribeSync("device.dev-1.command")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	req := map[string]any{
		"bundle_id": "core",
		"tool":      "send_command",
		"args": map[string]any{
			"uuid":    "dev-1",
			"command": "TurnOn",
		},
	}
	res := postJSON(t, ts.URL+"/api/mcp/call", req)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: got=%d want=%d", res.StatusCode, http.StatusOK)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected command message: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("invalid nats payload: %v", err)
	}
	if got["subject"] != "device.dev-1.command" {
		t.Fatalf("unexpected subject: %v", got["subject"])
	}
}

func TestHandlePluginHealth_AggregatesBundleHealthRPC(t *testing.T) {
	ns, nc := startTestNATS(t)
	defer ns.Shutdown()
	defer nc.Close()

	_, err := nc.Subscribe("bundle.discovery", func(msg *nats.Msg) {
		_ = nc.Publish(msg.Reply, []byte(`{"id":"plugin-automation","name":"Automation","status":"active"}`))
		_ = nc.Publish(msg.Reply, []byte(`{"id":"plugin-wiz","name":"Wiz","status":"active"}`))
	})
	if err != nil {
		t.Fatalf("subscribe bundle.discovery: %v", err)
	}
	_, err = nc.Subscribe("bundle.plugin-automation.health", func(msg *nats.Msg) {
		_ = nc.Publish(msg.Reply, []byte(`{"bundle_id":"plugin-automation","status":"ok","framework":{"nats_connected":true}}`))
	})
	if err != nil {
		t.Fatalf("subscribe bundle.plugin-automation.health: %v", err)
	}
	nc.Flush()

	ts := startTestAPI(t, nc)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/health/plugins")
	if err != nil {
		t.Fatalf("get health plugins: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: got=%d want=%d", res.StatusCode, http.StatusOK)
	}

	var got []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 bundle health rows, got %d", len(got))
	}

	var foundAutomation, foundWiz bool
	for _, row := range got {
		switch row["bundle_id"] {
		case "plugin-automation":
			foundAutomation = true
			if row["error"] != nil && row["error"] != "" {
				t.Fatalf("unexpected automation error: %v", row["error"])
			}
			health, _ := row["health"].(map[string]any)
			if health["status"] != "ok" {
				t.Fatalf("unexpected automation health: %v", health)
			}
		case "plugin-wiz":
			foundWiz = true
			if row["health"] != nil {
				t.Fatalf("expected wiz health to be absent, got %v", row["health"])
			}
			if row["error"] == nil || row["error"] == "" {
				t.Fatalf("expected wiz error for missing health responder")
			}
		}
	}
	if !foundAutomation || !foundWiz {
		t.Fatalf("missing expected bundle rows: automation=%v wiz=%v", foundAutomation, foundWiz)
	}
}
