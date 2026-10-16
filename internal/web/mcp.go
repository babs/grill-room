package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// The MCP server at /mcp (specs/012-agent-api.md, Phase 2): typed tools, each a sub-request to the
// REST routes with the caller's token, so their authorization is the routes' own (012-AC16).

// MCPResults are the outcomes grill_room_mcp_tool_calls_total counts.
var MCPResults = []string{"ok", "error", "server_error"}

const (
	msgMCPServerError = "Server error, try again"
	msgMCPHeld        = "Waiting for the AI host's approval"
)

// mcpArgs are a call's arguments, already checked against the tool's schema.
type mcpArgs map[string]any

func (a mcpArgs) str(k string) string { s, _ := a[k].(string); return s }

// id is an integer argument as a path segment; the schema made it an integer.
func (a mcpArgs) id(k string) string {
	f, _ := a[k].(float64)
	return strconv.FormatInt(int64(f), 10)
}

func (a mcpArgs) has(k string) bool { _, ok := a[k]; return ok }

// room is the path of the call's room.
func (a mcpArgs) room() string { return "/api/rooms/" + url.PathEscape(a.str("room")) }

// mcpTool is one tool of the catalogue: its schema, its hints, the routes it may reach and how it
// calls them.
type mcpTool struct {
	name, desc      string
	full            bool
	ro, destr, idem bool
	// schema holds the tool's "properties" and "required"; the rest of its JSON Schema is the same
	// for every tool.
	schema string
	// routes are the mux patterns its sub-requests may reach (012-AC21).
	routes []string
	run    func(c *mcpCall, a mcpArgs) (any, error)
}

// mcpCall is one tool call: the /mcp request it came in, whose token, address and context its
// sub-requests carry.
type mcpCall struct {
	s    *Server
	r    *http.Request
	tool *mcpTool
}

// toolError is a refusal the agent reads: a route's 4xx message, or a tool's own check.
type toolError string

func (e toolError) Error() string { return string(e) }

// errMCPServer is a sub-request that failed on the server's side: logged, never shown in detail.
var errMCPServer = errors.New(msgMCPServerError)

// subResponse records a sub-request's answer. SetReadDeadline lets the upload route set its
// deadline: the /mcp request's own read is over by then.
type subResponse struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (w *subResponse) Header() http.Header { return w.h }

func (w *subResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

func (w *subResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *subResponse) SetReadDeadline(time.Time) error { return nil }

// do sends one sub-request through the token gate and the routes, as the caller, and returns its
// JSON answer: {} for an empty one. A 4xx is a toolError with the route's message.
func (c *mcpCall) do(method, path string, body io.Reader, ctype string) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(c.r.Context(), method, path, body)
	if err != nil {
		return nil, 0, fmt.Errorf("mcp %s: %w", c.tool.name, err)
	}
	for _, h := range []string{"Authorization", "X-Forwarded-For", "X-Forwarded-Proto", "User-Agent"} {
		if v := c.r.Header.Values(h); len(v) > 0 {
			req.Header[h] = v
		}
	}
	req.RemoteAddr = c.r.RemoteAddr
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	// A tool reaches its catalogue routes and nothing else, whatever the arguments put in a path.
	if _, pattern := c.s.mux.Handler(req); !slices.Contains(c.tool.routes, pattern) {
		c.s.log.Error("mcp_route_refused", zap.String("tool", c.tool.name), zap.String("method", method), zap.String("pattern", pattern))
		return nil, 0, errMCPServer
	}
	w := &subResponse{h: http.Header{}}
	c.s.gate.ServeHTTP(w, req)
	switch {
	case w.status >= 500:
		c.s.log.Error("mcp_tool_failed", zap.String("tool", c.tool.name), zap.String("pattern", method+" "+path), zap.Int("status", w.status))
		return nil, w.status, errMCPServer
	case w.status >= 400:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(w.body.Bytes(), &e) != nil || e.Error == "" {
			e.Error = http.StatusText(w.status)
		}
		return nil, w.status, toolError(e.Error)
	case w.body.Len() == 0:
		return json.RawMessage(`{}`), w.status, nil
	}
	return json.RawMessage(bytes.TrimSpace(w.body.Bytes())), w.status, nil
}

// get, send and del are do for a read, a JSON body and a delete.
func (c *mcpCall) get(path string) (json.RawMessage, error) {
	b, _, err := c.do(http.MethodGet, path, nil, "")
	return b, err
}

func (c *mcpCall) send(method, path string, v any) (json.RawMessage, int, error) {
	if v == nil {
		return c.do(method, path, nil, "")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, 0, fmt.Errorf("mcp %s: %w", c.tool.name, err)
	}
	return c.do(method, path, bytes.NewReader(b), "application/json")
}

// pass sends v and answers the route's JSON as is.
func (c *mcpCall) pass(method, path string, v any) (any, error) {
	b, status, err := c.send(method, path, v)
	if err != nil {
		return nil, err
	}
	return held(b, status), nil
}

// held marks an answer the AI host must approve first: a 202, or a persona added pending
// (012-AC19).
func held(b json.RawMessage, status int) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if status != http.StatusAccepted && m["pending"] != true {
		return b
	}
	m["held"], m["status"] = true, msgMCPHeld
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// field reads one top-level field of a route's JSON answer into v.
func field(b json.RawMessage, key string, v any) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	if err := json.Unmarshal(m[key], v); err != nil {
		return fmt.Errorf("mcp: %s: %w", key, err)
	}
	return nil
}

// confirmed refuses a destructive call whose confirm is not the target's name, byte for byte,
// without saying the name: the agent reads it on purpose (012-AC20).
func confirmed(a mcpArgs, name, kind, readWith string) error {
	if a.str("confirm") != name {
		return toolError(fmt.Sprintf("confirm must be the exact name of the %s; read it with %s", kind, readWith))
	}
	return nil
}

const (
	pRoom        = "GET /api/rooms/{room}"
	pAgent       = "GET /api/rooms/{room}/agent"
	pDocuments   = "GET /api/rooms/{room}/documents"
	pStarter     = "GET /api/profile/starters/{starter}"
	pProfile     = "GET /api/profile"
	pPersonaSkl  = "/api/rooms/{room}/personas/{persona}/skills/{skill}"
	pPersonaPlug = "/api/rooms/{room}/personas/{persona}/plugins/{plugin}"
)

const roomArg = `"room":{"type":"string","description":"The room id, as list_rooms gives it."}`

// mcpTools is the catalogue, in the order tools/list gives it (012-AC15).
var mcpTools = []*mcpTool{
	{name: "list_rooms", desc: "List the rooms your profile is in, with their ids, names and whether you are a room admin there.",
		ro: true, idem: true, schema: `{}`, routes: []string{pProfile},
		run: func(c *mcpCall, _ mcpArgs) (any, error) {
			b, err := c.get("/api/profile")
			if err != nil {
				return nil, err
			}
			var p struct {
				Rooms json.RawMessage `json:"rooms"`
			}
			if err := field(b, "profile", &p); err != nil {
				return nil, err
			}
			return map[string]json.RawMessage{"rooms": p.Rooms}, nil
		}},
	{name: "get_room", desc: "Read a room: its participants, its AI personas with their settings, its queue and pending approvals.",
		ro: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `}}`, routes: []string{pRoom, pAgent},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			room, err := c.get(a.room())
			if err != nil {
				return nil, err
			}
			agent, err := c.get(a.room() + "/agent")
			if err != nil {
				return nil, err
			}
			return map[string]json.RawMessage{"room": room, "agent": agent}, nil
		}},
	{name: "list_documents", desc: "List a room's documents with their ids, names, formats and revision counts.",
		ro: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `}}`, routes: []string{pDocuments},
		run: func(c *mcpCall, a mcpArgs) (any, error) { return c.get(a.room() + "/documents") }},
	{name: "read_document", desc: "Read a document's text as Markdown: its latest revision, or the one given.",
		ro: true, idem: true, schema: `{"required":["room","document"],"properties":{` + roomArg + `,"document":{"type":"integer"},"revision":{"type":"integer","minimum":1}}}`,
		routes: []string{"GET /api/rooms/{room}/documents/{doc}/text"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			p := a.room() + "/documents/" + a.id("document") + "/text"
			if a.has("revision") {
				p += "?rev=" + a.id("revision")
			}
			return c.get(p)
		}},
	{name: "get_transcript", desc: "Read a room's chat and voice lines, oldest first, after the line id since. next is the since of the following page, 0 on the last one.",
		ro: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `,"since":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":500,"default":200}}}`,
		routes: []string{"GET /api/rooms/{room}/messages"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			return c.get(a.room() + "/messages?since=" + a.id("since") + "&limit=" + a.id("limit"))
		}},
	{name: "list_notes", desc: "List the notes (annotations) on a document, with their threads.",
		ro: true, idem: true, schema: `{"required":["room","document"],"properties":{` + roomArg + `,"document":{"type":"integer"}}}`,
		routes: []string{"GET /api/rooms/{room}/documents/{doc}/annotations"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			return c.get(a.room() + "/documents/" + a.id("document") + "/annotations")
		}},
	{name: "list_invites", desc: "List a room's invites: all of them for a room admin, your own otherwise.",
		ro: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `}}`, routes: []string{"GET /api/rooms/{room}/invites"},
		run: func(c *mcpCall, a mcpArgs) (any, error) { return c.get(a.room() + "/invites") }},
	{name: "list_starters", desc: "List the room starters you can create a room from: the built-ins, yours and the shared ones.",
		ro: true, idem: true, schema: `{}`, routes: []string{"GET /api/profile/starters"},
		run: func(c *mcpCall, _ mcpArgs) (any, error) { return c.get("/api/profile/starters") }},
	{name: "get_starter", desc: "Read a starter: its room name, fields to fill, personas, documents, skills and plugins. Starter 0 is Blank.",
		ro: true, idem: true, schema: `{"required":["starter"],"properties":{"starter":{"type":"integer","minimum":0},"version":{"type":"integer","minimum":1}}}`,
		routes: []string{pStarter},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			p := "/api/profile/starters/" + a.id("starter")
			if a.has("version") {
				p += "?v=" + a.id("version")
			}
			return c.get(p)
		}},
	{name: "list_templates", desc: "List the persona templates a room can add personas from.",
		ro: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `}}`, routes: []string{"GET /api/rooms/{room}/templates"},
		run: func(c *mcpCall, a mcpArgs) (any, error) { return c.get(a.room() + "/templates") }},
	{name: "list_skills", desc: "List the skills you can give a room's personas, or without a room, the skills you uploaded.",
		ro: true, idem: true, schema: `{"properties":{` + roomArg + `}}`, routes: []string{"GET /api/rooms/{room}/skills", "GET /api/profile/skills"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			if a.has("room") {
				return c.get(a.room() + "/skills")
			}
			return c.get("/api/profile/skills")
		}},
	{name: "list_plugins", desc: "List a room's plugins with their versions and which personas have them on.",
		ro: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `}}`, routes: []string{"GET /api/rooms/{room}/plugins"},
		run: func(c *mcpCall, a mcpArgs) (any, error) { return c.get(a.room() + "/plugins") }},

	{name: "create_room", desc: "Create a room you are the room admin of, blank or from a starter (fill its required fields). display_name is your name in it. Without a name, a starter's own room name is used.",
		full: true, schema: `{"required":["display_name"],"properties":{"name":{"type":"string"},"display_name":{"type":"string","minLength":1},"starter":{"type":"integer","minimum":1},"starter_version":{"type":"integer","minimum":1},"fields":{"type":"object","additionalProperties":{"type":"string"}}}}`,
		routes: []string{"POST /api/profile/rooms", pStarter},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			body := map[string]any{"name": a.str("name"), "display_name": a.str("display_name")}
			if !a.has("starter") {
				if strings.TrimSpace(a.str("name")) == "" {
					return nil, toolError("name is required for a blank room")
				}
				return c.pass(http.MethodPost, "/api/profile/rooms", body)
			}
			version := 0
			if a.has("starter_version") {
				version, _ = strconv.Atoi(a.id("starter_version"))
			}
			if strings.TrimSpace(a.str("name")) == "" {
				p := "/api/profile/starters/" + a.id("starter")
				if version > 0 {
					p += "?v=" + strconv.Itoa(version)
				}
				st, err := c.get(p)
				if err != nil {
					return nil, err
				}
				var m struct {
					RoomName string `json:"room_name"`
				}
				if err := field(st, "manifest", &m); err != nil {
					return nil, err
				}
				body["name"] = m.RoomName
			}
			id, _ := strconv.ParseInt(a.id("starter"), 10, 64)
			body["starter"] = map[string]any{"id": id, "version": version}
			if f, ok := a["fields"]; ok {
				body["fields"] = f
			}
			return c.pass(http.MethodPost, "/api/profile/rooms", body)
		}},
	{name: "add_persona", desc: "Add a persona to a room from a template (list_templates). Under host approval it waits for the AI host.",
		full: true, schema: `{"required":["room","template"],"properties":{` + roomArg + `,"template":{"type":"integer"}}}`,
		routes: []string{"POST /api/rooms/{room}/personas"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			id, _ := strconv.ParseInt(a.id("template"), 10, 64)
			return c.pass(http.MethodPost, a.room()+"/personas", map[string]any{"template_id": id})
		}},
	{name: "edit_persona", desc: "Change a room persona: name (one word), badge, colour, text (its instructions), model, effort, one automatic trigger (every_n turns, every_voice lines or every_minutes; 0 is off), self_call (can follow up on its own), watches_notes. Under host approval it waits for the AI host.",
		full: true, idem: true,
		schema: `{"required":["room","persona"],"properties":{` + roomArg + `,"persona":{"type":"integer"},"name":{"type":"string"},"badge":{"type":"string"},"colour":{"type":"string","pattern":"^#[0-9a-fA-F]{6}$"},"text":{"type":"string"},"model":{"enum":["haiku","sonnet","opus","fable"]},"effort":{"enum":["low","medium","high","xhigh","max"]},"every_n":{"type":"integer","minimum":0},"every_voice":{"type":"integer","minimum":0},"every_minutes":{"type":"integer","minimum":0},"self_call":{"type":"boolean"},"watches_notes":{"type":"boolean"}}}`,
		routes: []string{"PATCH /api/rooms/{room}/personas/{persona}"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			body := map[string]any{}
			for k, v := range a {
				if k != "room" && k != "persona" {
					body[k] = v
				}
			}
			return c.pass(http.MethodPatch, a.room()+"/personas/"+a.id("persona"), body)
		}},
	{name: "set_persona_skill", desc: "Turn a skill (list_skills) on or off for a persona. The first skill needs accept_disclaimer: skills run code in the AI host's sandbox.",
		full: true, idem: true, schema: `{"required":["room","persona","skill","on"],"properties":{` + roomArg + `,"persona":{"type":"integer"},"skill":{"type":"integer"},"on":{"type":"boolean"},"accept_disclaimer":{"type":"boolean"}}}`,
		routes: []string{http.MethodPut + " " + pPersonaSkl, http.MethodDelete + " " + pPersonaSkl},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			p := a.room() + "/personas/" + a.id("persona") + "/skills/" + a.id("skill")
			if a["on"] != true {
				return c.pass(http.MethodDelete, p, nil)
			}
			return c.pass(http.MethodPut, p, map[string]any{"accept_disclaimer": a["accept_disclaimer"] == true})
		}},
	{name: "set_persona_plugin", desc: "Turn a room plugin (list_plugins) on or off for a persona. A room admin's to do.",
		full: true, idem: true, schema: `{"required":["room","persona","plugin","on"],"properties":{` + roomArg + `,"persona":{"type":"integer"},"plugin":{"type":"integer"},"on":{"type":"boolean"}}}`,
		routes: []string{http.MethodPut + " " + pPersonaPlug, http.MethodDelete + " " + pPersonaPlug},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			p := a.room() + "/personas/" + a.id("persona") + "/plugins/" + a.id("plugin")
			if a["on"] != true {
				return c.pass(http.MethodDelete, p, nil)
			}
			return c.pass(http.MethodPut, p, nil)
		}},
	{name: "add_room_skill", desc: "Copy one of your personal skills (list_skills without a room) into a room. Under host approval it waits for the AI host.",
		full: true, schema: `{"required":["room","skill"],"properties":{` + roomArg + `,"skill":{"type":"integer"}}}`,
		routes: []string{"POST /api/rooms/{room}/skills"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			id, _ := strconv.ParseInt(a.id("skill"), 10, 64)
			return c.pass(http.MethodPost, a.room()+"/skills", map[string]any{"skill_id": id})
		}},
	{name: "check_plugin", desc: "Fetch a git repo over https and list the Claude Code plugins it holds, before add_plugin. A room admin's to do.",
		full: true, ro: true, idem: true, schema: `{"required":["room","url"],"properties":{` + roomArg + `,"url":{"type":"string","pattern":"^https://"},"ref":{"type":"string"}}}`,
		routes: []string{"POST /api/rooms/{room}/plugins/check"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			return c.pass(http.MethodPost, a.room()+"/plugins/check", map[string]any{"url": a.str("url"), "ref": a.str("ref")})
		}},
	{name: "add_plugin", desc: "Add a repo's plugins to a room, pinned at its current commit, on for the personas listed; a marketplace needs its entries named. The version waits for the AI host's inspection and approval when the room asks for it. A room admin's to do.",
		full: true, schema: `{"required":["room","url"],"properties":{` + roomArg + `,"url":{"type":"string","pattern":"^https://"},"ref":{"type":"string"},"entries":{"type":"array","items":{"type":"string"}},"personas":{"type":"array","items":{"type":"integer"}}}}`,
		routes: []string{"POST /api/rooms/{room}/plugins"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			body := map[string]any{"url": a.str("url"), "ref": a.str("ref")}
			for _, k := range []string{"entries", "personas"} {
				if v, ok := a[k]; ok {
					body[k] = v
				}
			}
			return c.pass(http.MethodPost, a.room()+"/plugins", body)
		}},
	{name: "save_room_as_starter", desc: "Save a room as a starter in your personal library: its personas, agenda, skills and plugins, and the documents named (all of them by default). A room admin's to do.",
		full: true, schema: `{"required":["room","title"],"properties":{` + roomArg + `,"title":{"type":"string","minLength":1},"description":{"type":"string"},"documents":{"type":"array","items":{"type":"string"}}}}`,
		routes: []string{"GET /api/rooms/{room}/starter-draft", "POST /api/rooms/{room}/starters"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			b, err := c.get(a.room() + "/starter-draft")
			if err != nil {
				return nil, err
			}
			var m store.StarterManifest
			if err := field(b, "manifest", &m); err != nil {
				return nil, err
			}
			names := func(n int, at func(int) string) []string {
				out := make([]string, n)
				for i := range n {
					out[i] = at(i)
				}
				return out
			}
			q := starterSaveReq{Title: a.str("title"), Description: a.str("description"), RoomName: m.RoomName, Agenda: m.Agenda, Fields: m.Fields, Personas: m.Personas,
				Documents: names(len(m.Documents), func(i int) string { return m.Documents[i].Name }),
				Skills:    names(len(m.Skills), func(i int) string { return m.Skills[i].Name }),
				Plugins:   names(len(m.Plugins), func(i int) string { return m.Plugins[i].Name })}
			if v, ok := a["documents"].([]any); ok {
				q.Documents = make([]string, 0, len(v))
				for _, d := range v {
					s, _ := d.(string)
					q.Documents = append(q.Documents, s)
				}
			}
			return c.pass(http.MethodPost, a.room()+"/starters", q)
		}},
	{name: "upload_document", desc: "Add a document to a room under name, from text or from base64 bytes, exactly one of the two. The name's extension sets the format: .md, .html, .pdf, .docx, .odt, .pptx, or an image (.png, .jpg, .gif, .webp, .svg).",
		full: true, schema: `{"required":["room","name"],"properties":{` + roomArg + `,"name":{"type":"string","minLength":1},"text":{"type":"string"},"content_base64":{"type":"string","contentEncoding":"base64"}}}`,
		routes: []string{"POST /api/rooms/{room}/documents"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			if a.has("text") == a.has("content_base64") {
				return nil, toolError("give exactly one of text and content_base64")
			}
			data := []byte(a.str("text"))
			if a.has("content_base64") {
				var err error
				if data, err = base64.StdEncoding.DecodeString(a.str("content_base64")); err != nil {
					return nil, toolError("content_base64 is not valid base64")
				}
			}
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			part, err := mw.CreateFormFile("file", a.str("name"))
			if err == nil {
				_, err = part.Write(data)
			}
			if err == nil {
				err = mw.Close()
			}
			if err != nil {
				return nil, fmt.Errorf("mcp upload: %w", err)
			}
			b, status, err := c.do(http.MethodPost, a.room()+"/documents", &body, mw.FormDataContentType())
			if err != nil {
				return nil, err
			}
			return held(b, status), nil
		}},
	{name: "get_invite_link", desc: "Get your shareable invite link to a room, made on the first call. It keeps working after this token is revoked.",
		full: true, idem: true, schema: `{"required":["room"],"properties":{` + roomArg + `}}`, routes: []string{"POST /api/rooms/{room}/invites/share"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			return c.pass(http.MethodPost, a.room()+"/invites/share", nil)
		}},

	{name: "delete_room", desc: "Delete a room with everything in it, for good. confirm must be the room's exact name. A room admin's to do.",
		full: true, destr: true, schema: `{"required":["room","confirm"],"properties":{` + roomArg + `,"confirm":{"type":"string"}}}`,
		routes: []string{pRoom, "DELETE /api/rooms/{room}"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			b, err := c.get(a.room())
			if err != nil {
				return nil, err
			}
			var room struct {
				Name string `json:"name"`
			}
			if err := field(b, "room", &room); err != nil {
				return nil, err
			}
			if err := confirmed(a, room.Name, "room", "get_room"); err != nil {
				return nil, err
			}
			return c.pass(http.MethodDelete, a.room(), nil)
		}},
	{name: "delete_document", desc: "Delete a document; a room admin can restore it from the room. confirm must be the document's exact name.",
		full: true, destr: true, schema: `{"required":["room","document","confirm"],"properties":{` + roomArg + `,"document":{"type":"integer"},"confirm":{"type":"string"}}}`,
		routes: []string{pDocuments, "DELETE /api/rooms/{room}/documents/{doc}"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			b, err := c.get(a.room() + "/documents")
			if err != nil {
				return nil, err
			}
			var docs []store.Document
			if err := json.Unmarshal(b, &docs); err != nil {
				return nil, fmt.Errorf("mcp: %w", err)
			}
			i := slices.IndexFunc(docs, func(d store.Document) bool { return strconv.FormatInt(d.ID, 10) == a.id("document") })
			if i < 0 {
				return nil, toolError("no such document")
			}
			if err := confirmed(a, docs[i].Name, "document", "list_documents"); err != nil {
				return nil, err
			}
			return c.pass(http.MethodDelete, a.room()+"/documents/"+a.id("document"), nil)
		}},
	{name: "remove_persona", desc: "Remove a persona from a room. confirm must be the persona's exact name.",
		full: true, destr: true, schema: `{"required":["room","persona","confirm"],"properties":{` + roomArg + `,"persona":{"type":"integer"},"confirm":{"type":"string"}}}`,
		routes: []string{pAgent, "DELETE /api/rooms/{room}/personas/{persona}"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			b, err := c.get(a.room() + "/agent")
			if err != nil {
				return nil, err
			}
			var ps []store.RoomPersona
			if err := field(b, "personas", &ps); err != nil {
				return nil, err
			}
			i := slices.IndexFunc(ps, func(p store.RoomPersona) bool { return strconv.FormatInt(p.ID, 10) == a.id("persona") })
			if i < 0 {
				return nil, toolError("no such persona")
			}
			if err := confirmed(a, ps[i].Name, "persona", "get_room"); err != nil {
				return nil, err
			}
			return c.pass(http.MethodDelete, a.room()+"/personas/"+a.id("persona"), nil)
		}},
	{name: "revoke_invite", desc: "Revoke an invite: whoever joined only through it loses the room. confirm must be the invite's id, as list_invites gives it.",
		full: true, destr: true, schema: `{"required":["room","invite","confirm"],"properties":{` + roomArg + `,"invite":{"type":"integer"},"confirm":{"type":"string"}}}`,
		routes: []string{"DELETE /api/rooms/{room}/invites/{invite}"},
		run: func(c *mcpCall, a mcpArgs) (any, error) {
			if err := confirmed(a, a.id("invite"), "invite", "list_invites"); err != nil {
				return nil, err
			}
			return c.pass(http.MethodDelete, a.room()+"/invites/"+a.id("invite"), nil)
		}},
}

// toolSchema is a tool's full input schema (012-AC15).
func toolSchema(t *mcpTool) json.RawMessage {
	var s map[string]any
	if err := json.Unmarshal([]byte(t.schema), &s); err != nil {
		panic(fmt.Sprintf("mcp tool %s: schema: %v", t.name, err))
	}
	s["$schema"], s["type"], s["additionalProperties"] = "https://json-schema.org/draft/2020-12/schema", "object", false
	if _, ok := s["properties"]; !ok {
		s["properties"] = map[string]any{}
	}
	b, _ := json.Marshal(s) // plain maps: cannot fail
	return b
}

// mcpCounters counts the tool calls by tool and result.
type mcpCounters map[string]map[string]*atomic.Int64

func newMCPCounters() mcpCounters {
	m := mcpCounters{}
	for _, t := range mcpTools {
		m[t.name] = map[string]*atomic.Int64{}
		for _, r := range MCPResults {
			m[t.name][r] = new(atomic.Int64)
		}
	}
	return m
}

// MCPTools names the tools, for the metrics.
func MCPTools() []string {
	out := make([]string, len(mcpTools))
	for i, t := range mcpTools {
		out[i] = t.name
	}
	return out
}

// MCPToolCalls counts the calls of tool that ended in result (MCPResults).
func (s *Server) MCPToolCalls(tool, result string) int64 { return s.mcpCalls[tool][result].Load() }

// newMCPServer is the MCP server a token of scope sees: a read-only one lists the read tools only
// (012-AC15).
func (s *Server) newMCPServer(full bool) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "grill-room", Title: "grill-room", Version: s.opts.Version}, &mcp.ServerOptions{
		Instructions: "grill-room rooms, as your profile: read them and set them up (personas, documents, skills, plugins). Read a target's name before a destructive call: confirm must match it.",
	})
	for _, t := range mcpTools {
		if t.full && !full {
			continue
		}
		destr := t.destr
		tool := &mcp.Tool{Name: t.name, Description: t.desc, InputSchema: toolSchema(t),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t.ro, DestructiveHint: &destr, IdempotentHint: t.idem}}
		mcp.AddTool(srv, tool, func(_ context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			var r *http.Request
			if req.Extra != nil && req.Extra.TokenInfo != nil {
				r, _ = req.Extra.TokenInfo.Extra[mcpRequestKey].(*http.Request)
			}
			if r == nil {
				return nil, nil, errMCPServer
			}
			out, err := t.run(&mcpCall{s: s, r: r, tool: t}, in)
			result := "ok"
			switch {
			case errors.Is(err, errMCPServer):
				result = "server_error"
			case err != nil:
				result = "error"
			}
			s.mcpCalls[t.name][result].Add(1)
			if err != nil {
				return nil, nil, err
			}
			// structuredContent is an object: a route's list goes under items.
			if b, ok := out.(json.RawMessage); ok && len(b) > 0 && b[0] == '[' {
				out = map[string]json.RawMessage{"items": b}
			}
			return nil, out, nil
		})
	}
	return srv
}

// mcpRequestKey carries the /mcp request to the tools, in the token info the SDK hands them.
const mcpRequestKey = "request"

// mcpHandler serves /mcp: an API token only, as a bare 401 otherwise, so a client starts no OAuth
// flow (012-AC14); JSON answers, no session (012-AC13); foreign origins refused.
func (s *Server) mcpHandler() http.Handler {
	read, full := s.newMCPServer(false), s.newMCPServer(true)
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if t := apiTokenOf(r); t != nil && t.Scope == store.ScopeFull {
			return full
		}
		return read
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
		// The server listens on loopback behind its proxy: the SDK's Host check would refuse every
		// proxied request. The bearer token, which a rebinding page cannot send, covers it.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        s.opts.MaxUploadBytes*4/3 + 64<<10})
	// The gate checked the token already: this only hands the request to the tools. No metadata
	// URL, so no WWW-Authenticate.
	withRequest := auth.RequireBearerToken(func(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		t := apiTokenOf(r)
		if t == nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: strconv.FormatInt(t.ProfileID, 10), Extra: map[string]any{mcpRequestKey: r}}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)
	cop := http.NewCrossOriginProtection()
	return cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiTokenOf(r) == nil {
			httpError(w, http.StatusUnauthorized, "an API token is required: Authorization: Bearer grt_…")
			return
		}
		withRequest.ServeHTTP(w, r)
	}))
}
