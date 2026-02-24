package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/slidebolt/plugin-sdk"
	"os"
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
					{"name": "list_devices", "description": "Read all IoT devices"},
					{"name": "get_device", "description": "Read a single device by UUID"},
					{"name": "delete_device", "description": "Delete a device by UUID"},
					{"name": "update_device_raw", "description": "Update raw data for a device"},

					{"name": "create_entity", "description": "Create a new entity on a device"},
					{"name": "get_entity", "description": "Read an entity by UUID"},
					{"name": "update_entity_raw", "description": "Update raw data for an entity"},
					{"name": "delete_entity", "description": "Delete an entity by UUID"},

					{"name": "get_entity_script", "description": "Read script for an entity"},
					{"name": "set_entity_script", "description": "Update script for an entity"},
					{"name": "get_bundle_script", "description": "Read script for a bundle (plugin)"},
					{"name": "set_bundle_script", "description": "Update script for a bundle (plugin)"},

					{"name": "get_plugin_config", "description": "Read config/raw data for a plugin"},
					{"name": "update_plugin_config", "description": "Update config for a plugin bundle"},
					{"name": "send_command", "description": "Execute a command on a device/entity"},
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

func (s *Server) resolveBundle(uuid string) string {
	obj, ok := sdk.GetByUUID(sdk.UUID(uuid))
	if !ok {
		return ""
	}
	// Try device first
	if dev, ok := obj.(sdk.Device); ok {
		return string(dev.BundleID())
	}
	// Try entity (requires type cast or SDK update)
	// For now, assume it's a map/struct with BundleID if retrieved via SDK
	return ""
}

func (s *Server) handleToolCall(id interface{}, params map[string]interface{}) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]interface{})

	switch name {
	case "list_devices":
		devs := sdk.GetDevices()
		b, _ := json.MarshalIndent(devs, "", "  ")
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(b)}}})
	case "get_device":
		uuid, _ := args["uuid"].(string)
		dev, ok := sdk.GetByUUID(sdk.UUID(uuid))
		if !ok {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Error: device not found"}}})
			return
		}
		b, _ := json.MarshalIndent(dev, "", "  ")
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(b)}}})
	case "update_device_raw":
		uuid, _ := args["uuid"].(string)
		raw, _ := args["raw"].(map[string]interface{})
		bundleID := s.resolveBundle(uuid)
		if bundleID == "" {
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Error: could not resolve bundle for device"}}})
			return
		}
		payload, _ := json.Marshal(map[string]any{"device_uuid": uuid, "raw": raw})
		s.nc.Publish(fmt.Sprintf("bundle.%s.update_device_raw", bundleID), payload)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Device raw updated"}}})
	case "create_entity":
		devUUID, _ := args["device_uuid"].(string)
		etype, _ := args["type"].(string)
		name, _ := args["name"].(string)
		sid, _ := args["source_id"].(string)
		bundleID := s.resolveBundle(devUUID)
		payload, _ := json.Marshal(map[string]any{"device_uuid": devUUID, "type": etype, "name": name, "source_id": sid})
		resp, _ := s.nc.Request(fmt.Sprintf("bundle.%s.create_entity", bundleID), payload, 2*time.Second)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(resp.Data)}}})
	case "get_entity":
		uuid, _ := args["uuid"].(string)
		bundleID := s.resolveBundle(uuid)
		payload, _ := json.Marshal(map[string]any{"uuid": uuid})
		resp, _ := s.nc.Request(fmt.Sprintf("bundle.%s.get_entity", bundleID), payload, 2*time.Second)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(resp.Data)}}})
	case "update_entity_raw":
		uuid, _ := args["uuid"].(string)
		raw, _ := args["raw"].(map[string]interface{})
		bundleID := s.resolveBundle(uuid)
		payload, _ := json.Marshal(map[string]any{"entity_uuid": uuid, "raw": raw})
		s.nc.Publish(fmt.Sprintf("bundle.%s.update_entity_raw", bundleID), payload)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Entity raw updated"}}})
	case "delete_entity":
		uuid, _ := args["uuid"].(string)
		bundleID := s.resolveBundle(uuid)
		payload, _ := json.Marshal(map[string]any{"uuid": uuid})
		s.nc.Request(fmt.Sprintf("bundle.%s.delete_entity", bundleID), payload, 2*time.Second)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Entity deleted"}}})
	case "get_entity_script":
		uuid, _ := args["uuid"].(string)
		bundleID := s.resolveBundle(uuid)
		resp, err := s.nc.Request(fmt.Sprintf("bundle.%s.get_entity_script", bundleID), []byte(uuid), 2*time.Second)
		if err != nil || resp == nil {
			msg := "get_entity_script request failed"
			if err != nil {
				msg = fmt.Sprintf("get_entity_script request failed: %v", err)
			}
			s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": msg}}})
			return
		}
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(resp.Data)}}})
	case "set_entity_script":
		uuid, _ := args["uuid"].(string)
		script, _ := args["script"].(string)
		bundleID := s.resolveBundle(uuid)
		payload, _ := json.Marshal(map[string]any{"entity_uuid": uuid, "script": script})
		s.nc.Publish(fmt.Sprintf("bundle.%s.set_entity_script", bundleID), payload)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Entity script updated"}}})
	case "get_bundle_script":
		bundleID, _ := args["bundle_id"].(string)
		resp, _ := s.nc.Request(fmt.Sprintf("bundle.%s.get_script", bundleID), nil, 2*time.Second)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(resp.Data)}}})
	case "set_bundle_script":
		bundleID, _ := args["bundle_id"].(string)
		script, _ := args["script"].(string)
		s.nc.Request(fmt.Sprintf("bundle.%s.set_script", bundleID), []byte(script), 2*time.Second)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Bundle script updated"}}})
	case "get_plugin_config":
		bundleID, _ := args["bundle_id"].(string)
		resp, _ := s.nc.Request(fmt.Sprintf("bundle.%s.get_raw", bundleID), nil, 2*time.Second)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": string(resp.Data)}}})
	case "update_plugin_config":
		bundleID, _ := args["bundle_id"].(string)
		config, _ := args["config"].(map[string]interface{})
		payload, _ := json.Marshal(config)
		s.nc.Publish(fmt.Sprintf("bundle.%s.configure", bundleID), payload)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Plugin config updated"}}})
	case "send_command":
		uuid, _ := args["uuid"].(string)
		cmd, _ := args["command"].(string)
		prefix := "device"
		if obj, ok := sdk.GetByUUID(sdk.UUID(uuid)); ok {
			if _, isEntity := obj.(sdk.Entity); isEntity {
				prefix = "entity"
			}
		}
		subject := fmt.Sprintf("%s.%s.command", prefix, uuid)
		payload := make(map[string]interface{})
		for k, v := range args {
			if k == "payload" {
				if nested, ok := v.(map[string]interface{}); ok {
					for nk, nv := range nested {
						payload[nk] = nv
					}
					continue
				}
			}
			payload[k] = v
		}
		if _, ok := payload["command"]; !ok {
			payload["command"] = cmd
		}
		data, _ := json.Marshal(sdk.Message{Subject: subject, Payload: payload})
		s.nc.Publish(subject, data)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Command Sent"}}})
	case "delete_device":
		uuid, _ := args["uuid"].(string)
		bundleID := s.resolveBundle(uuid)
		payload, _ := json.Marshal(map[string]any{"uuid": uuid})
		s.nc.Publish(fmt.Sprintf("bundle.%s.delete_device", bundleID), payload)
		s.respond(id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "Device deletion signal sent"}}})
	}
}
