package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"plugin-sdk"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

type Server struct {
	nc *nats.Conn
	mu sync.Mutex
}

func NewServer(nc *nats.Conn) *Server {
	return &Server{nc: nc}
}

func (s *Server) Run() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case "initialize":
			s.respond(id, map[string]interface{}{"protocolVersion": "2024-11-05"})
		case "tools/list":
			s.respond(id, map[string]interface{}{
				"tools": []map[string]interface{}{
					{"name": "list_devices", "description": "List all IoT devices"},
					{"name": "send_command", "description": "Send command to device"},
					{"name": "delete_device", "description": "Delete a device by UUID"},
					{"name": "set_entity_script", "description": "Set script for an entity UUID and hot-reload it"},
					{"name": "configure_plugin", "description": "Update persistent configuration for a plugin bundle"},
				},
			})
		case "tools/call":
			s.handleToolCall(id, req["params"].(map[string]interface{}))
		}
	}
}

func (s *Server) respond(id interface{}, result interface{}) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}

func (s *Server) handleToolCall(id interface{}, params map[string]interface{}) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]interface{})

	switch name {
	case "list_devices":
		devs := sdk.GetDevices()
		var list []map[string]interface{}
		for _, d := range devs {
			m := d.Metadata()
			list = append(list, map[string]interface{}{
				"uuid": m.ID,
				"name": m.Name,
				"sid":  m.SourceID,
			})
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(b)}}})
	case "send_command":
		uuid, _ := args["uuid"].(string)
		cmd, _ := args["command"].(string)
		subject := fmt.Sprintf("device.%s.command", uuid)
		payload := map[string]interface{}{"command": cmd}
		data, _ := json.Marshal(sdk.Message{Subject: subject, Payload: payload})
		s.nc.Publish(subject, data)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Command Sent"}}})
	case "delete_device":
		uuid, _ := args["uuid"].(string)
		if uuid == "" {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Error: uuid is required"}}})
			return
		}
		// stdio MCP server runs in core process only; deletion is served by HTTP MCP path.
		// Keep this as a compatibility no-op signal for clients that accidentally hit stdio.
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "delete_device is available on HTTP MCP endpoint"}}})
	case "set_entity_script":
		entityUUID, _ := args["entity_uuid"].(string)
		if entityUUID == "" {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Error: entity_uuid is required"}}})
			return
		}
		// stdio MCP server runs in core process only; script updates are served by HTTP MCP path.
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "set_entity_script is available on HTTP MCP endpoint"}}})
	case "configure_plugin":
		bundleID, _ := args["bundle_id"].(string)
		config, _ := args["config"].(map[string]interface{})

		if bundleID == "" {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Error: bundle_id is required"}}})
			return
		}

		payload, _ := json.Marshal(config)
		respMsg, err := s.nc.Request(fmt.Sprintf("bundle.%s.configure", bundleID), payload, 2*time.Second)
		if err != nil {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": fmt.Sprintf("Error: configure request failed: %v", err)}}})
			return
		}

		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respMsg.Data, &resp); err == nil && !resp.OK {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": fmt.Sprintf("Error: configure failed: %s", resp.Error)}}})
			return
		}

		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": fmt.Sprintf("Configuration updated for %s", bundleID)}}})
	}
}
